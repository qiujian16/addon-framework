package agentdeploy

import (
	"context"
	"fmt"

	"github.com/open-cluster-management/addon-framework/pkg/addoninterface"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	clusterinformers "github.com/open-cluster-management/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlister "github.com/open-cluster-management/api/client/cluster/listers/cluster/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

type addonConfigController struct {
	addonClient           addonv1alpha1client.Interface
	addonLister           addonlisterv1alpha1.ManagedClusterAddOnLister
	managedClusterLister  clusterlister.ManagedClusterLister
	addonName             string
	signer                string
	enableRegistration    bool
	addonInstallNamespace string
	agentAddon            addoninterface.AgentAddonWithRegistration
}

func NewAddonConfigController(
	addonClient addonv1alpha1client.Interface,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	clusterInformers clusterinformers.ManagedClusterInformer,
	addonName string,
	enableRegistration bool,
	addonInstallNamespace string,
	recorder events.Recorder,
	agentAddon addoninterface.AgentAddonWithRegistration,
) factory.Controller {
	controller := &addonConfigController{
		addonClient:           addonClient,
		addonLister:           addonInformers.Lister(),
		managedClusterLister:  clusterInformers.Lister(),
		addonName:             addonName,
		addonInstallNamespace: addonInstallNamespace,
		enableRegistration:    enableRegistration,
		agentAddon:            agentAddon,
	}

	return factory.New().
		WithFilteredEventsInformersQueueKeyFunc(
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
		WithSync(controller.sync).ToController(fmt.Sprintf("%s-addon-config-controller", addonName), recorder)
}

func (c *addonConfigController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	addonNameSpace := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling addon config %q in ns %q", c.addonName, addonNameSpace)
	addon, err := c.addonLister.ManagedClusterAddOns(addonNameSpace).Get(c.addonName)
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}

	cluster, err := c.managedClusterLister.Get(addonNameSpace)
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}

	annotationData := map[string]string{}
	if c.enableRegistration {
		annotationData["signer"] = c.signer
		annotationData["installNamespace"] = c.addonInstallNamespace
		annotationData["enable_registration"] = "true"
	} else {
		annotationData["enable_registration"] = "false"
	}

	// Generate Bootstrap kubeconfig
	kubeConfigData, err := c.agentAddon.AgentBootstrapKubeConfig(cluster)
	if err != nil {
		return err
	}

	if len(kubeConfigData) > 0 {
		annotationData["bootstrapSecret"] = fmt.Sprintf("%s-bootstrap-kubeconfig", c.addonName)
	}
	modified := resourcemerge.BoolPtr(false)
	addonCopy := addon.DeepCopy()

	resourcemerge.MergeMap(modified, &addonCopy.Annotations, annotationData)
	if !*modified {
		return nil
	}

	_, err = c.addonClient.AddonV1alpha1().ManagedClusterAddOns(addonNameSpace).Update(ctx, addonCopy, metav1.UpdateOptions{})
	return err
}
