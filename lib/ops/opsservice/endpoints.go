package opsservice

import (
	"fmt"
	"strings"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/gravitational/trace"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GetKubeClient lazy initializes K8s client
func (o *Operator) GetKubeClient() (*kubernetes.Clientset, error) {
	o.kubeMutex.Lock()
	defer o.kubeMutex.Unlock()

	if o.kubeClient != nil {
		return o.kubeClient, nil
	}

	client, _, err := utils.GetKubeClient("")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	o.kubeClient = client
	return o.kubeClient, nil
}

// GetApplicationEndpoints returns a list of application endpoints for a deployed site
func (o *Operator) GetApplicationEndpoints(key ops.SiteKey) ([]ops.Endpoint, error) {
	site, err := o.openSite(key)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if len(site.app.Manifest.Endpoints) == 0 {
		return nil, nil
	}

	client, err := o.GetKubeClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// query for nodes, we might need them later on
	nodeList, err := client.Core().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	namespaceList, err := client.Core().Namespaces().List(metav1.ListOptions{})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var endpoints []ops.Endpoint
	for _, e := range site.app.Manifest.Endpoints {
		if e.Hidden {
			continue
		}

		var serviceList *v1.ServiceList
		for _, ns := range namespaceList.Items {
			services, err := client.Core().Services(ns.Name).List(metav1.ListOptions{
				LabelSelector: utils.MakeSelector(e.Selector).String(),
			})
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if serviceList == nil {
				serviceList = services
			} else {
				serviceList.Items = append(serviceList.Items, services.Items...)
			}
		}

		if serviceList == nil {
			continue
		}

		var addresses []string
		for _, service := range serviceList.Items {
			serviceAddresses, err := getAddresses(service, nodeList)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			for _, a := range serviceAddresses {
				// only select matching endpoints if they match the port, or the port is not specified
				if e.Port == 0 || strings.HasSuffix(a, fmt.Sprintf(":%d", e.Port)) {
					if e.Protocol != "" {
						a = fmt.Sprintf("%v://%v", e.Protocol, a)
					}
					addresses = append(addresses, a)
				}
			}
		}

		if len(addresses) > 0 {
			endpoints = append(endpoints, ops.Endpoint{
				Name:        e.Name,
				Description: e.Description,
				Addresses:   addresses,
			})
		}
	}

	return endpoints, nil
}

// getAddresses returns a list of URLs the provided service can be reached at
//
// It follows the following logic:
//   - if the service has an attached load balancer, its address(-es) are returned;
//   - otherwise, if the service is exposed on nodes' ports, their addresses are returned;
//   - otherwise, a "cluster IP" is returned.
func getAddresses(service v1.Service, nodeList *v1.NodeList) (addresses []string, err error) {
	// if there're load balancers, grab'em
	if len(service.Status.LoadBalancer.Ingress) > 0 {
		for _, ingress := range service.Status.LoadBalancer.Ingress {
			for _, port := range service.Spec.Ports {
				addresses = append(addresses, fmt.Sprintf("%v:%v", ingress.Hostname, port.Port))
			}
		}
		return addresses, nil
	}

	// otherwise see if the services is exposed on nodes
	var nodePorts []int
	for _, port := range service.Spec.Ports {
		if port.NodePort != 0 {
			nodePorts = append(nodePorts, int(port.NodePort))
		}
	}
	if len(nodePorts) > 0 {
		var externalIPs, internalIPs []string
		for _, node := range nodeList.Items {
			for _, address := range node.Status.Addresses {
				if address.Type == constants.KubeNodeExternalIP {
					externalIPs = append(externalIPs, address.Address)
				}
				if address.Type == constants.KubeNodeInternalIP {
					internalIPs = append(internalIPs, address.Address)
				}
			}
		}

		if len(externalIPs) > 0 {
			for _, ip := range externalIPs {
				for _, port := range nodePorts {
					addresses = append(addresses, fmt.Sprintf("%v:%v", ip, port))
				}
			}
			return addresses, nil
		}

		if len(internalIPs) > 0 {
			for _, ip := range internalIPs {
				for _, port := range nodePorts {
					addresses = append(addresses, fmt.Sprintf("%v:%v", ip, port))
				}
			}
			return addresses, nil
		}
	}
	// fall back to cluster IP
	for _, port := range service.Spec.Ports {
		addresses = append(addresses,
			fmt.Sprintf("%v:%v", service.Spec.ClusterIP, port.Port))
	}
	return addresses, nil
}