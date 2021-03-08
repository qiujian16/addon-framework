package lease

import (
	"context"
	"time"

	"github.com/open-cluster-management/addon-framework/pkg/helpers"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	clusterv1informer "github.com/open-cluster-management/api/client/cluster/informers/externalversions/cluster/v1"
	clusterv1listers "github.com/open-cluster-management/api/client/cluster/listers/cluster/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	coordv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	coordinformers "k8s.io/client-go/informers/coordination/v1"
	"k8s.io/client-go/kubernetes"
	coordlisters "k8s.io/client-go/listers/coordination/v1"
	"k8s.io/utils/pointer"
)

const leaseDurationTimes = 5

// leaseController checks the lease of managed clusters on hub cluster to determine whether a managed cluster is available.
type leaseController struct {
	kubeClient    kubernetes.Interface
	addonClient   addonv1alpha1client.Interface
	addonLister   addonlisterv1alpha1.ManagedClusterAddOnLister
	clusterLister clusterv1listers.ManagedClusterLister
	leaseLister   coordlisters.LeaseLister
}

// NewClusterLeaseController creates a cluster lease controller on hub cluster.
func NewAddonLeaseController(
	kubeClient kubernetes.Interface,
	addonClient addonv1alpha1client.Interface,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	leaseInformer coordinformers.LeaseInformer,
	clusterInformer clusterv1informer.ManagedClusterInformer,
	resyncInterval time.Duration,
	recorder events.Recorder) factory.Controller {
	c := &leaseController{
		kubeClient:  kubeClient,
		addonClient: addonClient,
		addonLister: addonInformers.Lister(),
		leaseLister: leaseInformer.Lister(),
	}
	return factory.New().
		WithInformers(leaseInformer.Informer()).
		WithSync(c.sync).
		ResyncEvery(resyncInterval).
		ToController("ManagedClusterLeaseController", recorder)
}

// sync checks the lease of each accepted cluster on hub to determine whether a managed cluster is available.
func (c *leaseController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	clusters, err := c.clusterLister.List(labels.Everything())
	if err != nil {
		return nil
	}
	for _, cluster := range clusters {
		// get the lease of a cluster, if the lease is not found, create it
		leaseName := "addon-lease"
		observedLease, err := c.leaseLister.Leases(cluster.Name).Get(leaseName)
		switch {
		case errors.IsNotFound(err):
			lease := &coordv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      leaseName,
					Namespace: cluster.Name,
					Labels:    map[string]string{"addon.open-cluster-management.io/cluster-name": cluster.Name},
				},
				Spec: coordv1.LeaseSpec{
					HolderIdentity: pointer.StringPtr(leaseName),
					RenewTime:      &metav1.MicroTime{Time: time.Now()},
				},
			}
			if _, err := c.kubeClient.CoordinationV1().Leases(cluster.Name).Create(ctx, lease, metav1.CreateOptions{}); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}

		leaseDurationSeconds := cluster.Spec.LeaseDurationSeconds
		// for backward compatible, release-2.1 has mutating admission webhook to mutate this field,
		// but release-2.0 does not have the mutating admission webhook
		if leaseDurationSeconds == 0 {
			leaseDurationSeconds = 60
		}

		gracePeriod := time.Duration(leaseDurationTimes*leaseDurationSeconds) * time.Second
		// the lease is constantly updated, do nothing
		now := time.Now()
		if now.Before(observedLease.Spec.RenewTime.Add(gracePeriod)) {
			continue
		}

		addons, err := c.addonLister.ManagedClusterAddOns(cluster.Name).List(labels.Everything())
		if err != nil {
			return err
		}

		for _, addon := range addons {
			// the lease is not constantly updated, update it to unknown
			conditionUpdateFn := helpers.UpdateAddonConditionFn(metav1.Condition{
				Type:    "Available",
				Status:  metav1.ConditionUnknown,
				Reason:  "AddonManagerUpdateStopped",
				Message: "Addon manager stopped updating its lease.",
			})
			_, updated, err := helpers.UpdateAddonStatus(ctx, c.addonClient, cluster.Name, addon.Name, conditionUpdateFn)
			if err != nil {
				return err
			}
			if updated {
				syncCtx.Recorder().Eventf("AddonAvailableConditionUpdated",
					"update addon %q of managed cluster %q available condition to unknown, due to its lease is not updated constantly",
					addon.Name, cluster.Name)
			}
		}

	}
	return nil
}
