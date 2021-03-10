package lease

import (
	"context"
	"fmt"
	"time"

	"github.com/open-cluster-management/addon-framework/pkg/helpers"
	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	coordv1 "k8s.io/api/coordination/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	coordinformers "k8s.io/client-go/informers/coordination/v1"
	coordlisters "k8s.io/client-go/listers/coordination/v1"
)

const (
	addonLeaseDurationTimes   = 5
	AddonLeaseDurationSeconds = 60
)

// leaseController checks the lease of managed clusters on hub cluster to determine whether a managed cluster is available.
type addonLeaseController struct {
	clusterName string
	addonClient addonv1alpha1client.Interface
	addonLister addonlisterv1alpha1.ManagedClusterAddOnLister
	leaseLister coordlisters.LeaseLister
}

// NewClusterLeaseController creates a cluster lease controller on hub cluster.
func NewAddonLeaseController(
	clusterName string,
	addonClient addonv1alpha1client.Interface,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	leaseInformer coordinformers.LeaseInformer,
	resyncInterval time.Duration,
	recorder events.Recorder) factory.Controller {
	c := &addonLeaseController{
		clusterName: clusterName,
		addonClient: addonClient,
		addonLister: addonInformers.Lister(),
		leaseLister: leaseInformer.Lister(),
	}
	return factory.New().
		WithInformers(addonInformers.Informer(), leaseInformer.Informer()).
		WithSync(c.sync).
		ResyncEvery(resyncInterval).
		ToController("ManagedClusterLeaseController", recorder)
}

// sync checks the lease of each accepted cluster on hub to determine whether a managed cluster is available.
func (c *addonLeaseController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	addons, err := c.addonLister.ManagedClusterAddOns(c.clusterName).List(labels.Everything())
	if err != nil {
		return nil
	}
	for _, addon := range addons {
		var conditionFn helpers.UpdateAddonStatusFunc
		lease, err := c.getLeaseByAddon(addon)
		switch {
		case err != nil:
			conditionFn = helpers.UpdateAddonConditionFn(
				metav1.Condition{
					Type:    "Available",
					Status:  metav1.ConditionFalse,
					Reason:  "AddonLeaseNotFound",
					Message: "Addon agent is not found.",
				},
			)
		case lease == nil:
			return nil
		default:
			conditionFn = helpers.UpdateAddonConditionFn(c.checkAddonLeases(lease))
		}

		_, updated, err := helpers.UpdateAddonStatus(ctx, c.addonClient, c.clusterName, addon.Name, conditionFn)
		if err != nil {
			return err
		}
		if updated {
			syncCtx.Recorder().Eventf("AddonAvailableConditionUpdated",
				"update addon for %q cluster %q available condition to unknown, due to its lease is not updated constantly",
				addon.Name, c.clusterName)
		}
	}
	return nil
}

func (c *addonLeaseController) getLeaseByAddon(addon *addonapiv1alpha1.ManagedClusterAddOn) (*coordv1.Lease, error) {
	if len(addon.Annotations) == 0 {
		return nil, nil
	}
	installNamespace := addon.Annotations["installNamespace"]
	if len(installNamespace) == 0 {
		return nil, nil
	}
	lease, err := c.leaseLister.Leases(installNamespace).Get(fmt.Sprintf("open-cluster-management-addon-%s", addon.Name))
	if err != nil {
		return nil, err
	}

	return lease, nil
}

// Check all addon leases, return degraded=False if one lease is valid
func (c *addonLeaseController) checkAddonLeases(lease *coordv1.Lease) metav1.Condition {
	now := time.Now()
	gracePeriod := time.Duration(addonLeaseDurationTimes*AddonLeaseDurationSeconds) * time.Second
	if now.Before(lease.Spec.RenewTime.Add(gracePeriod)) {
		return metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionTrue,
			Reason:  "ManagedClusterLeaseUpdated",
			Message: "Addon agent is updating its lease.",
		}
	}

	return metav1.Condition{
		Type:    "Available",
		Status:  metav1.ConditionFalse,
		Reason:  "AddonLeaseUpdateStopped",
		Message: "Addon agent stopped updating its lease.",
	}
}
