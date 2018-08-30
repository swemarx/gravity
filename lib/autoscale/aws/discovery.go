package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/ops"

	"github.com/gravitational/trace"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PublishDiscovery periodically updates discovery information
func (a *Autoscaler) PublishDiscovery(ctx context.Context, operator ops.Operator) {
	err := a.syncDiscovery(ctx, operator)
	if err != nil {
		a.Errorf("Failed to publish discovery: %v.", trace.DebugReport(err))
	}
	ticker := time.NewTicker(defaults.DiscoveryPublishInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err = a.syncDiscovery(ctx, operator)
			if err != nil {
				a.Errorf("Failed to publish discovery: %v.", trace.DebugReport(err))
			}
		}
	}
}

// syncDiscovery syncs cluster discovery information in the SSM
func (a *Autoscaler) syncDiscovery(ctx context.Context, operator ops.Operator) error {
	cluster, err := operator.GetLocalSite()
	if err != nil {
		return trace.Wrap(err)
	}

	if err := a.syncToken(ctx, operator, cluster); err != nil {
		return trace.Wrap(err)
	}

	if err := a.syncMasterService(ctx); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (a *Autoscaler) syncToken(ctx context.Context, operator ops.Operator, cluster *ops.Site) error {
	joinToken, err := operator.GetExpandToken(cluster.Key())
	if err != nil {
		return trace.Wrap(err)
	}
	if err := a.publishJoinToken(ctx, joinToken.Token); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func (a *Autoscaler) getServiceURL() (string, error) {
	service, err := a.Client.Core().Services(constants.KubeSystemNamespace).Get(constants.GravityServiceName, v1.GetOptions{})
	if err != nil {
		return "", trace.Wrap(err)
	}
	var port int32
	for _, p := range service.Spec.Ports {
		if p.Name == constants.GravityServicePortName {
			port = p.Port
			break
		}
	}
	if port == 0 {
		return "", trace.NotFound("no port %q found for service %q", constants.GravityServicePortName, constants.GravityServiceName)
	}
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.Hostname != "" {
			return fmt.Sprintf("https://%v:%v", ingress.Hostname, port), nil
		}
	}
	return "", trace.NotFound("ingress load balancer not found for %v", constants.GravityServiceName)
}

func (a *Autoscaler) syncMasterService(ctx context.Context) error {
	serviceURL, err := a.getServiceURL()
	if err != nil {
		return trace.Wrap(err)
	}
	return a.publishServiceURL(ctx, serviceURL)
}