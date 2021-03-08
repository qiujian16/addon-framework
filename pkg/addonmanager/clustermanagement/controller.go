package clustermanagement

import (
	"context"
	"fmt"
	"time"

	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

// CreatingControllerSyncInterval is exposed so that integration tests can crank up the constroller sync speed.
var CreatingControllerSyncInterval = 60 * time.Minute

type clusterManagementController struct {
	addonName               string
	addonClient             addonv1alpha1client.Interface
	clusterManagementLister addonlisterv1alpha1.ClusterManagementAddOnLister
}

func NewClusterManagementController(
	addonClient addonv1alpha1client.Interface,
	addonInformers addoninformerv1alpha1.ClusterManagementAddOnInformer,
	addonName string,
	recorder events.Recorder,
) factory.Controller {
	c := &clusterManagementController{
		addonName:               addonName,
		addonClient:             addonClient,
		clusterManagementLister: addonInformers.Lister(),
	}

	return factory.New().WithFilteredEventsInformersQueueKeyFunc(
		func(obj runtime.Object) string {
			accessor, _ := meta.Accessor(obj)
			return accessor.GetNamespace()
		},
		func(obj interface{}) bool {
			accessor, _ := meta.Accessor(obj)
			if accessor.GetName() != addonName {
				return false
			}
			return true
		}, addonInformers.Informer()).
		WithSync(c.sync).
		ResyncEvery(CreatingControllerSyncInterval).
		ToController(fmt.Sprintf("%s-addon-deploy-controller", addonName), recorder)
}

func (c *clusterManagementController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	clusterManagementName := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling clustermanagement %q", clusterManagementName)

	_, err := c.clusterManagementLister.Get(c.addonName)
	switch {
	case errors.IsNotFound(err):
		clusterManagement := &addonapiv1alpha1.ClusterManagementAddOn{
			ObjectMeta: metav1.ObjectMeta{
				Name: c.addonName,
			},
			Spec: addonapiv1alpha1.ClusterManagementAddOnSpec{},
		}
		_, err := c.addonClient.AddonV1alpha1().ClusterManagementAddOns().Create(ctx, clusterManagement, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	}

	return nil
}
