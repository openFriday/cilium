// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ipcache

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/ip"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/labels/cidr"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/source"
)

// AllocateCIDRs attempts to allocate identities for a list of CIDRs. If any
// allocation fails, all allocations are rolled back and the error is returned.
// When an identity is freshly allocated for a CIDR, it is added to the
// ipcache if 'newlyAllocatedIdentities' is 'nil', otherwise the newly allocated
// identities are placed in 'newlyAllocatedIdentities' and it is the caller's
// responsibility to upsert them into ipcache by calling UpsertGeneratedIdentities().
//
// Previously used numeric identities for the given prefixes may be passed in as the
// 'oldNIDs' parameter; nil slice must be passed if no previous numeric identities exist.
// Previously used NID is allocated if still available. Non-availability is not an error.
//
// Upon success, the caller must also arrange for the resulting identities to
// be released via a subsequent call to ReleaseCIDRIdentitiesByCIDR().
func (ipc *IPCache) AllocateCIDRs(
	prefixes []*net.IPNet, oldNIDs []identity.NumericIdentity, newlyAllocatedIdentities map[string]*identity.Identity,
) ([]*identity.Identity, error) {
	// maintain list of used identities to undo on error
	usedIdentities := make([]*identity.Identity, 0, len(prefixes))

	// Maintain list of newly allocated identities to update ipcache,
	// but upsert them to ipcache only if no map was given by the caller.
	upsert := false
	if newlyAllocatedIdentities == nil {
		upsert = true
		newlyAllocatedIdentities = map[string]*identity.Identity{}
	}

	ipc.metadata.RLock()
	ipc.Lock()
	allocatedIdentities := make(map[string]*identity.Identity, len(prefixes))
	for i, p := range prefixes {
		if p == nil {
			continue
		}

		lbls := cidr.GetCIDRLabels(p)
		lbls.MergeLabels(ipc.metadata.getLocked(p.IP.String()))
		oldNID := identity.InvalidIdentity
		if oldNIDs != nil && len(oldNIDs) > i {
			oldNID = oldNIDs[i]
		}
		id, isNew, err := ipc.allocate(p, lbls, oldNID)
		if err != nil {
			ipc.IdentityAllocator.ReleaseSlice(context.Background(), nil, usedIdentities)
			ipc.Unlock()
			ipc.metadata.RUnlock()
			return nil, err
		}

		prefixStr := p.String()
		usedIdentities = append(usedIdentities, id)
		allocatedIdentities[prefixStr] = id
		if isNew {
			newlyAllocatedIdentities[prefixStr] = id
		}
	}
	ipc.Unlock()
	ipc.metadata.RUnlock()

	// Only upsert into ipcache if identity wasn't allocated
	// before and the caller does not care doing this
	if upsert {
		ipc.UpsertGeneratedIdentities(newlyAllocatedIdentities, nil)
	}

	identities := make([]*identity.Identity, 0, len(allocatedIdentities))
	for _, id := range allocatedIdentities {
		identities = append(identities, id)
	}
	return identities, nil
}

// AllocateCIDRsForIPs performs the same action as AllocateCIDRs but for IP
// addresses instead of CIDRs.
//
// Upon success, the caller must also arrange for the resulting identities to
// be released via a subsequent call to ReleaseCIDRIdentitiesByID().
func (ipc *IPCache) AllocateCIDRsForIPs(
	prefixes []net.IP, newlyAllocatedIdentities map[string]*identity.Identity,
) ([]*identity.Identity, error) {
	return ipc.AllocateCIDRs(ip.GetCIDRPrefixesFromIPs(prefixes), nil, newlyAllocatedIdentities)
}

func cidrLabelToPrefix(label string) (string, bool) {
	if !strings.HasPrefix(label, labels.LabelSourceCIDR) {
		return "", false
	}
	return strings.TrimPrefix(label, labels.LabelSourceCIDR+":"), true
}

// UpsertGeneratedIdentities unconditionally upserts 'newlyAllocatedIdentities'
// into the ipcache, then also upserts any CIDR identities in 'usedIdentities'
// that were not already upserted. If any 'usedIdentities' are upserted, these
// are counted separately as they may provide an indication of another logic
// error elsewhere in the codebase that is causing premature ipcache deletions.
func (ipc *IPCache) UpsertGeneratedIdentities(newlyAllocatedIdentities map[string]*identity.Identity, usedIdentities []*identity.Identity) {
	for prefixString, id := range newlyAllocatedIdentities {
		ipc.Upsert(prefixString, nil, 0, nil, Identity{
			ID:     id.ID,
			Source: source.Generated,
		})
	}
	if len(usedIdentities) == 0 {
		return
	}

	toUpsert := make(map[string]*identity.Identity)
	ipc.mutex.RLock()
	for _, id := range usedIdentities {
		prefix, ok := cidrLabelToPrefix(id.CIDRLabel.String())
		if !ok {
			log.WithFields(logrus.Fields{
				logfields.Identity: id.ID,
			}).Warning("BUG: Attempting to upsert non-CIDR identity")
			continue
		}
		if _, ok := ipc.LookupByIPRLocked(prefix); ok {
			// Already there; continue
			continue
		}
		toUpsert[prefix] = id
	}
	ipc.mutex.RUnlock()
	for prefix, id := range toUpsert {
		metrics.IPCacheErrorsTotal.WithLabelValues(
			metricTypeRecover, metricErrorUnexpected,
		).Inc()
		ipc.Upsert(prefix, nil, 0, nil, Identity{
			ID:     id.ID,
			Source: source.Generated,
		})
	}
}

// allocate will allocate a new identity for the given prefix based on the
// given set of labels. This function performs both global and local (CIDR)
// identity allocation and the set of labels determine which identity
// allocation type is to occur.
//
// If the identity is a CIDR identity, then its corresponding Identity will
// have its CIDR labels set correctly.
//
// A possible previously used numeric identity for these labels can be passed
// in as the 'oldNID' parameter; identity.InvalidIdentity must be passed if no
// previous numeric identity exists.
//
// It is up to the caller to provide the full set of labels for identity
// allocation.
func (ipc *IPCache) allocate(prefix *net.IPNet, lbls labels.Labels, oldNID identity.NumericIdentity) (*identity.Identity, bool, error) {
	if prefix == nil {
		return nil, false, nil
	}

	allocateCtx, cancel := context.WithTimeout(context.Background(), option.Config.IPAllocationTimeout)
	defer cancel()

	id, isNew, err := ipc.IdentityAllocator.AllocateIdentity(allocateCtx, lbls, false, oldNID)
	if err != nil {
		return nil, isNew, fmt.Errorf("failed to allocate identity for cidr %s: %s", prefix, err)
	}

	if lbls.Has(labels.LabelWorld[labels.IDNameWorld]) {
		id.CIDRLabel = labels.NewLabelsFromModel([]string{labels.LabelSourceCIDR + ":" + prefix.String()})
	}

	return id, isNew, err
}

func (ipc *IPCache) releaseCIDRIdentities(ctx context.Context, prefixes []string) {
	// Create a critical section for identity release + removal from ipcache.
	// Otherwise, it's possible to trigger the following race condition:
	//
	// Goroutine 1                | Goroutine 2
	// releaseCIDRIdentities()    | AllocateCIDRs()
	// -> Release(..., id, ...)   |
	//                            | -> allocate(...)
	//                            | -> ipc.UpsertGeneratedIdentities(...)
	// -> ipc.deleteLocked(...)   |
	//
	// In this case, the expectation from Goroutine 2 is that an identity
	// is allocated and that identity is in the ipcache, but the result
	// is that the identity is allocated but the ipcache entry is missing.
	ipc.Lock()
	defer ipc.Unlock()

	toDelete := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		_, c, err := net.ParseCIDR(prefix)
		if err != nil {
			log.WithFields(logrus.Fields{
				logfields.CIDR: c,
			}).WithError(err).Error("Unable to parse CIDR during ipcache release")
			continue
		}
		lbls := cidr.GetCIDRLabels(c)
		id := ipc.IdentityAllocator.LookupIdentity(ctx, lbls)
		if id == nil {
			log.WithFields(logrus.Fields{
				logfields.CIDR: prefix,
			}).Errorf("Unable to find identity of previously used CIDR")
			continue
		}
		released, err := ipc.IdentityAllocator.Release(ctx, id, false)
		if err != nil {
			log.WithFields(logrus.Fields{
				logfields.Identity: id,
				logfields.CIDR:     prefix,
			}).WithError(err).Warning("Unable to release CIDR identity. Ignoring error. Identity may be leaked")
		}
		if released {
			toDelete = append(toDelete, prefix)
		}
	}

	for _, prefix := range toDelete {
		ipc.deleteLocked(prefix, source.Generated)
	}
}

// ReleaseCIDRIdentitiesByCIDR releases the identities of a list of CIDRs.
// When the last use of the identity is released, the ipcache entry is deleted.
func (ipc *IPCache) ReleaseCIDRIdentitiesByCIDR(nets []*net.IPNet) {
	prefixes := make([]string, 0, len(nets))
	for _, n := range nets {
		prefixes = append(prefixes, n.String())
	}
	ipc.deferredPrefixRelease.enqueue(prefixes, "cidr-prefix-release")
}

// ReleaseCIDRIdentitiesByID releases the specified identities.
// When the last use of the identity is released, the ipcache entry is deleted.
func (ipc *IPCache) ReleaseCIDRIdentitiesByID(ctx context.Context, identities []identity.NumericIdentity) {
	prefixes := make([]string, 0, len(identities))
	for _, nid := range identities {
		if id := ipc.IdentityAllocator.LookupIdentityByID(ctx, nid); id != nil {
			prefix, ok := cidrLabelToPrefix(id.CIDRLabel.String())
			if !ok {
				log.WithFields(logrus.Fields{
					logfields.Identity: nid,
					logfields.Labels:   id.Labels,
				}).Warn("Unexpected release of non-CIDR identity, will leak this identity. Please report this issue to the developers.")
				continue
			}
			prefixes = append(prefixes, prefix)
		} else {
			log.WithFields(logrus.Fields{
				logfields.Identity: nid,
			}).Warn("Unexpected release of numeric identity that is no longer allocated")
		}
	}

	ipc.deferredPrefixRelease.enqueue(prefixes, "selector-prefix-release")
}
