// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Package k8s abstracts all Kubernetes specific behaviour
package k8s

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"

	"github.com/cilium/cilium/pkg/backoff"
	"github.com/cilium/cilium/pkg/controller"
	ipamOption "github.com/cilium/cilium/pkg/ipam/option"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	k8sconfig "github.com/cilium/cilium/pkg/k8s/config"
	k8sConst "github.com/cilium/cilium/pkg/k8s/constants"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	k8sversion "github.com/cilium/cilium/pkg/k8s/version"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/node"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/source"
)

const (
	nodeRetrievalMaxRetries = 15
)

type k8sGetter interface {
	GetK8sNode(ctx context.Context, nodeName string) (*corev1.Node, error)
	GetCiliumNode(ctx context.Context, nodeName string) (*ciliumv2.CiliumNode, error)
}

func waitForNodeInformation(ctx context.Context, k8sGetter k8sGetter, nodeName string) *nodeTypes.Node {
	backoff := backoff.Exponential{
		Min:    time.Duration(200) * time.Millisecond,
		Max:    2 * time.Minute,
		Factor: 2.0,
		Name:   "k8s-node-retrieval",
	}

	for retry := 0; retry < nodeRetrievalMaxRetries; retry++ {
		n, err := retrieveNodeInformation(ctx, k8sGetter, nodeName)
		if err != nil {
			log.WithError(err).Warning("Waiting for k8s node information")
			backoff.Wait(ctx)
			continue
		}

		return n
	}

	return nil
}

func retrieveNodeInformation(ctx context.Context, nodeGetter k8sGetter, nodeName string) (*nodeTypes.Node, error) {
	requireIPv4CIDR := option.Config.K8sRequireIPv4PodCIDR
	requireIPv6CIDR := option.Config.K8sRequireIPv6PodCIDR
	// At this point it's not clear whether the device auto-detection will
	// happen, as initKubeProxyReplacementOptions() might disable BPF NodePort.
	// Anyway, to be on the safe side, don't give up waiting for a (Cilium)Node
	// self object.
	mightAutoDetectDevices := option.MightAutoDetectDevices()
	var n *nodeTypes.Node

	if option.Config.IPAM == ipamOption.IPAMClusterPool || option.Config.IPAM == ipamOption.IPAMClusterPoolV2 {
		ciliumNode, err := nodeGetter.GetCiliumNode(ctx, nodeName)
		if err != nil {
			// If no CIDR is required, retrieving the node information is
			// optional
			if !requireIPv4CIDR && !requireIPv6CIDR && !mightAutoDetectDevices {
				return nil, nil
			}

			return nil, fmt.Errorf("unable to retrieve CiliumNode: %s", err)
		}

		no := nodeTypes.ParseCiliumNode(ciliumNode)
		n = &no
		log.WithField(logfields.NodeName, n.Name).Info("Retrieved node information from cilium node")
	} else {
		k8sNode, err := nodeGetter.GetK8sNode(ctx, nodeName)
		if err != nil {
			// If no CIDR is required, retrieving the node information is
			// optional
			if !requireIPv4CIDR && !requireIPv6CIDR && !mightAutoDetectDevices {
				return nil, nil
			}

			return nil, fmt.Errorf("unable to retrieve k8s node information: %s", err)

		}

		nodeInterface := ConvertToNode(k8sNode)
		if nodeInterface == nil {
			// This will never happen and the GetNode on line 63 will be soon
			// make a request from the local store instead.
			return nil, fmt.Errorf("invalid k8s node: %s", k8sNode)
		}
		typesNode := nodeInterface.(*slim_corev1.Node)

		// The source is left unspecified as this node resource should never be
		// used to update state
		n = ParseNode(typesNode, source.Unspec)
		log.WithField(logfields.NodeName, n.Name).Info("Retrieved node information from kubernetes node")
	}

	if requireIPv4CIDR && n.IPv4AllocCIDR == nil {
		return nil, fmt.Errorf("required IPv4 PodCIDR not available")
	}

	if requireIPv6CIDR && n.IPv6AllocCIDR == nil {
		return nil, fmt.Errorf("required IPv6 PodCIDR not available")
	}

	return n, nil
}

// useNodeCIDR sets the ipv4-range and ipv6-range values values from the
// addresses defined in the given node.
func useNodeCIDR(n *nodeTypes.Node) {
	if n.IPv4AllocCIDR != nil && option.Config.EnableIPv4 {
		node.SetIPv4AllocRange(n.IPv4AllocCIDR)
	}
	if n.IPv6AllocCIDR != nil && option.Config.EnableIPv6 {
		node.SetIPv6NodeRange(n.IPv6AllocCIDR)
	}
}

// Init initializes the Kubernetes package. It is required to call Configure()
// beforehand.
func Init(conf k8sconfig.Configuration) error {
	restConfig, err := CreateConfig()
	if err != nil {
		return fmt.Errorf("unable to create k8s client rest configuration: %s", err)
	}

	defaultCloseAllConns := setDialer(restConfig)

	// Use the same http client for all k8s connections. It does not matter that
	// we are using a restConfig for the HTTP client that differs from each
	// individual client since the rest.HTTPClientFor only does not use fields
	// that are specific for each client, for example:
	// restConfig.ContentConfig.ContentType.
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return fmt.Errorf("unable to create k8s REST client: %s", err)
	}

	k8sRestClient, err := createDefaultClient(restConfig, httpClient)
	if err != nil {
		return fmt.Errorf("unable to create k8s client: %s", err)
	}

	err = createDefaultCiliumClient(restConfig, httpClient)
	if err != nil {
		return fmt.Errorf("unable to create cilium k8s client: %s", err)
	}

	if err := createAPIExtensionsClient(restConfig, httpClient); err != nil {
		return fmt.Errorf("unable to create k8s apiextensions client: %s", err)
	}

	// We are implementing the same logic as Kubelet, see
	// https://github.com/kubernetes/kubernetes/blob/v1.24.0-beta.0/cmd/kubelet/app/server.go#L852.
	var closeAllConns func()
	if s := os.Getenv("DISABLE_HTTP2"); len(s) > 0 {
		closeAllConns = defaultCloseAllConns
	} else {
		closeAllConns = func() {
			utilnet.CloseIdleConnectionsFor(restConfig.Transport)
		}
	}

	heartBeat := func(ctx context.Context) error {
		// Kubernetes does a get node of the node that kubelet is running [0]. This seems excessive in
		// our case because the amount of data transferred is bigger than doing a Get of /healthz.
		// For this reason we have picked to perform a get on `/healthz` instead a get of a node.
		//
		// [0] https://github.com/kubernetes/kubernetes/blob/v1.17.3/pkg/kubelet/kubelet_node_status.go#L423
		res := k8sRestClient.Get().Resource("healthz").Do(ctx)
		return res.Error()
	}

	if option.Config.K8sHeartbeatTimeout != 0 {
		controller.NewManager().UpdateController("k8s-heartbeat",
			controller.ControllerParams{
				DoFunc: func(context.Context) error {
					runHeartbeat(
						heartBeat,
						option.Config.K8sHeartbeatTimeout,
						closeAllConns,
					)
					return nil
				},
				RunInterval: option.Config.K8sHeartbeatTimeout,
			},
		)
	}

	if err := k8sversion.Update(Client(), conf); err != nil {
		return err
	}

	if !k8sversion.Capabilities().MinimalVersionMet {
		return fmt.Errorf("k8s version (%v) is not meeting the minimal requirement (%v)",
			k8sversion.Version(), k8sversion.MinimalVersionConstraint)
	}

	return nil
}

// WaitForNodeInformation retrieves the node information via the CiliumNode or
// Kubernetes Node resource. This function will block until the information is
// received. k8sGetter is a function used to retrieve the node from either
// the kube-apiserver or a local cache, depending on the caller.
func WaitForNodeInformation(ctx context.Context, k8sGetter k8sGetter) error {
	// Use of the environment variable overwrites the node-name
	// automatically derived
	nodeName := nodeTypes.GetName()
	if nodeName == "" {
		if option.Config.K8sRequireIPv4PodCIDR || option.Config.K8sRequireIPv6PodCIDR {
			return fmt.Errorf("node name must be specified via environment variable '%s' to retrieve Kubernetes PodCIDR range", k8sConst.EnvNodeNameSpec)
		}
		if option.MightAutoDetectDevices() {
			log.Info("K8s node name is empty. BPF NodePort might not be able to auto detect all devices")
		}
		return nil
	}

	if n := waitForNodeInformation(ctx, k8sGetter, nodeName); n != nil {
		nodeIP4 := n.GetNodeIP(false)
		nodeIP6 := n.GetNodeIP(true)

		k8sNodeIP := n.GetK8sNodeIP()

		log.WithFields(logrus.Fields{
			logfields.NodeName:         n.Name,
			logfields.Labels:           logfields.Repr(n.Labels),
			logfields.IPAddr + ".ipv4": nodeIP4,
			logfields.IPAddr + ".ipv6": nodeIP6,
			logfields.V4Prefix:         n.IPv4AllocCIDR,
			logfields.V6Prefix:         n.IPv6AllocCIDR,
			logfields.K8sNodeIP:        k8sNodeIP,
		}).Info("Received own node information from API server")

		useNodeCIDR(n)

		// Note: Node IPs are derived regardless of
		// option.Config.EnableIPv4 and
		// option.Config.EnableIPv6. This is done to enable
		// underlay addressing to be different from overlay
		// addressing, e.g. an IPv6 only PodCIDR running over
		// IPv4 encapsulation.
		if nodeIP4 != nil {
			node.SetIPv4(nodeIP4)
		}

		if nodeIP6 != nil {
			node.SetIPv6(nodeIP6)
		}

		node.SetLabels(n.Labels)

		node.SetK8sExternalIPv4(n.GetExternalIP(false))
		node.SetK8sExternalIPv6(n.GetExternalIP(true))

		// K8s Node IP is used by BPF NodePort devices auto-detection
		node.SetK8sNodeIP(k8sNodeIP)

		restoreRouterHostIPs(n)
	} else {
		// if node resource could not be received, fail if
		// PodCIDR requirement has been requested
		if option.Config.K8sRequireIPv4PodCIDR || option.Config.K8sRequireIPv6PodCIDR {
			log.Fatal("Unable to derive PodCIDR via Node or CiliumNode resource, giving up")
		}
	}

	// Annotate addresses will occur later since the user might
	// want to specify them manually
	return nil
}

// restoreRouterHostIPs restores (sets) the router IPs found from the
// Kubernetes resource.
//
// Note that it does not validate the correctness of the IPs, as that is done
// later in the daemon initialization when node.AutoComplete() is called.
func restoreRouterHostIPs(n *nodeTypes.Node) {
	if !option.Config.EnableHostIPRestore {
		return
	}

	router4 := n.GetCiliumInternalIP(false)
	router6 := n.GetCiliumInternalIP(true)
	if router4 != nil {
		node.SetInternalIPv4Router(router4)
	}
	if router6 != nil {
		node.SetIPv6Router(router6)
	}
	if router4 != nil || router6 != nil {
		log.WithFields(logrus.Fields{
			logfields.IPv4: router4,
			logfields.IPv6: router6,
		}).Info("Restored router IPs from node information")
	}
}
