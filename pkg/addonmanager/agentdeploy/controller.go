package agentdeploy

import (
	"context"
	"fmt"

	"github.com/open-cluster-management/addon-framework/pkg/addoninterface"
	"github.com/open-cluster-management/addon-framework/pkg/helpers"
	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	clusterinformers "github.com/open-cluster-management/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlister "github.com/open-cluster-management/api/client/cluster/listers/cluster/v1"
	workv1client "github.com/open-cluster-management/api/client/work/clientset/versioned"
	workinformers "github.com/open-cluster-management/api/client/work/informers/externalversions/work/v1"
	worklister "github.com/open-cluster-management/api/client/work/listers/work/v1"
	clusterv1 "github.com/open-cluster-management/api/cluster/v1"
	workapiv1 "github.com/open-cluster-management/api/work/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// AddonWorkLabel is to label the manifestowk relates to the addon
const (
	AddonWorkLabel = "open-cluster-management.io/addon-name"
	addonFinalizer = "addon.open-cluster-management.io/work-cleanup"
)

// managedClusterController reconciles instances of ManagedCluster on the hub.
type addonDeployController struct {
	workClient                workv1client.Interface
	addonClient               addonv1alpha1client.Interface
	kubeClient                kubernetes.Interface
	managedClusterLister      clusterlister.ManagedClusterLister
	managedClusterAddonLister addonlisterv1alpha1.ManagedClusterAddOnLister
	workLister                worklister.ManifestWorkLister
	configLister              cache.GenericLister
	agentAddon                addoninterface.AgentAddon
	addonName                 string
}

func NewAddonDeployController(
	workClient workv1client.Interface,
	addonClient addonv1alpha1client.Interface,
	kubeClient kubernetes.Interface,
	clusterInformers clusterinformers.ManagedClusterInformer,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	workInformers workinformers.ManifestWorkInformer,
	crInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	configurationGVR *schema.GroupVersionResource,
	agentAddon addoninterface.AgentAddon,
	addonName string,
	recorder events.Recorder,
) factory.Controller {
	c := &addonDeployController{
		workClient:                workClient,
		addonClient:               addonClient,
		kubeClient:                kubeClient,
		managedClusterLister:      clusterInformers.Lister(),
		managedClusterAddonLister: addonInformers.Lister(),
		workLister:                workInformers.Lister(),
		addonName:                 addonName,
		agentAddon:                agentAddon,
	}

	ctrlFactory := factory.New().WithFilteredEventsInformersQueueKeyFunc(
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
		WithInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			}, clusterInformers.Informer()).
		WithSync(c.sync)
	if configurationGVR != nil {
		configInformer := crInformerFactory.ForResource(*configurationGVR)
		c.configLister = configInformer.Lister()
		ctrlFactory = ctrlFactory.WithInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetNamespace()
			}, configInformer.Informer())
	}
	return ctrlFactory.ToController(fmt.Sprintf("%s-addon-deploy-controller", addonName), recorder)
}

func (c *addonDeployController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	clusterName := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling addon deploy %q", clusterName)

	// Get ManagedCluster
	managedCluster, err := c.managedClusterLister.Get(clusterName)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	managedClusterAddon, err := c.managedClusterAddonLister.ManagedClusterAddOns(clusterName).Get(c.addonName)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	managedClusterAddon = managedClusterAddon.DeepCopy()
	if managedClusterAddon.DeletionTimestamp.IsZero() {
		hasFinalizer := false
		for i := range managedClusterAddon.Finalizers {
			if managedClusterAddon.Finalizers[i] == addonFinalizer {
				hasFinalizer = true
				break
			}
		}
		if !hasFinalizer {
			managedClusterAddon.Finalizers = append(managedClusterAddon.Finalizers, addonFinalizer)
			_, err := c.addonClient.AddonV1alpha1().ManagedClusterAddOns(clusterName).Update(ctx, managedClusterAddon, metav1.UpdateOptions{})
			return err
		}
	}

	// managedClusterAddon is deleting, we remove its related resources
	if !managedClusterAddon.DeletionTimestamp.IsZero() {
		if err := c.removeAddonResources(ctx, managedCluster); err != nil {
			return err
		}
		return c.removeAddonFinalizer(ctx, managedClusterAddon)
	}

	// Get clusterManagement to find related configuration
	clusterManagement, err := c.addonClient.AddonV1alpha1().ClusterManagementAddOns().Get(ctx, c.addonName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Read addon config
	config, err := c.getAddonConfig(
		clusterManagement.Spec.AddOnConfiguration.CRDName, clusterManagement.Spec.AddOnConfiguration.CRName, clusterName)
	if err != nil {
		return err
	}

	objects, err := c.agentAddon.AgentManifests(managedCluster, config)
	work, err := c.buildManifestWorkFromObject(clusterName, objects)
	if err != nil {
		return err
	}

	if work == nil {
		return nil
	}

	// apply work
	existingWork, err := c.workClient.WorkV1().ManifestWorks(clusterName).Get(ctx, work.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err := c.workClient.WorkV1().ManifestWorks(clusterName).Create(ctx, work, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	if helpers.ManifestsEqual(existingWork.Spec.Workload.Manifests, work.Spec.Workload.Manifests) {
		return nil
	}
	work.ResourceVersion = existingWork.ResourceVersion
	_, err = c.workClient.WorkV1().ManifestWorks(clusterName).Update(ctx, work, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return err
}

func (c *addonDeployController) getAddonConfig(crdName, crName, clusterName string) (runtime.Object, error) {
	if crdName == "" || crName == "" {
		return nil, nil
	}

	config, err := c.configLister.ByNamespace(clusterName).Get(crName)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func (c *addonDeployController) buildManifestWorkFromObject(cluster string, objects []runtime.Object) (*workapiv1.ManifestWork, error) {
	work := &workapiv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("addon-%s-deploy", c.addonName),
			Namespace: cluster,
			Labels: map[string]string{
				AddonWorkLabel: c.addonName,
			},
		},
		Spec: workapiv1.ManifestWorkSpec{
			Workload: workapiv1.ManifestsTemplate{
				Manifests: []workapiv1.Manifest{},
			},
		},
	}

	if len(objects) == 0 {
		return nil, nil
	}

	for _, object := range objects {
		rawObject, err := runtime.Encode(unstructured.UnstructuredJSONScheme, object)
		if err != nil {
			return nil, err
		}
		work.Spec.Workload.Manifests = append(work.Spec.Workload.Manifests, workapiv1.Manifest{
			RawExtension: runtime.RawExtension{Raw: rawObject},
		})
	}

	return work, nil
}

func (c *addonDeployController) removeAddonResources(ctx context.Context, cluster *clusterv1.ManagedCluster) error {
	// remove work
	selector, err := labels.NewRequirement(AddonWorkLabel, selection.Equals, []string{c.addonName})
	if err != nil {
		panic(err)
	}
	works, err := c.workLister.ManifestWorks(cluster.Name).List(labels.NewSelector().Add(*selector))
	if err != nil {
		return err
	}

	if len(works) == 0 {
		return nil
	}

	for _, work := range works {
		if work.DeletionTimestamp.IsZero() {
			c.workClient.WorkV1().ManifestWorks(cluster.Name).Delete(ctx, work.Name, metav1.DeleteOptions{})
		}
	}

	return fmt.Errorf("%d of work relating to the addon %s is still being deleted", len(works), c.addonName)
}

func (c *addonDeployController) removeAddonFinalizer(ctx context.Context, addon *addonapiv1alpha1.ManagedClusterAddOn) error {
	copiedFinalizers := []string{}
	for i := range addon.Finalizers {
		if addon.Finalizers[i] == addonFinalizer {
			continue
		}
		copiedFinalizers = append(copiedFinalizers, addon.Finalizers[i])
	}

	if len(addon.Finalizers) != len(copiedFinalizers) {
		addon.Finalizers = copiedFinalizers
		_, err := c.addonClient.AddonV1alpha1().ManagedClusterAddOns(addon.Namespace).Update(ctx, addon, metav1.UpdateOptions{})
		return err
	}

	return nil
}
