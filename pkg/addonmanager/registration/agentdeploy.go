package registration

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"

	"github.com/ghodss/yaml"
	"github.com/open-cluster-management/addon-framework/pkg/addoninterface"
	"github.com/open-cluster-management/addon-framework/pkg/addonmanager/registration/bindata"
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
	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	operatorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var registrationAgentFiles = []string{
	"pkg/manager/controllers/registration/manifests/addon-registration-bootstrap-secret.yaml",
}

var registrationoHubFiles = []string{
	"pkg/manager/controllers/registration/manifests/addon-hub-registration-clusterrole.yaml",
	"pkg/manager/controllers/registration/manifests/addon-hub-registration-clusterrolebinding.yaml",
}

const registrationFinalizer = "addon.open-cluster-management.io/registration-cleanup"

// managedClusterController reconciles instances of ManagedCluster on the hub.
type registrationAgentDeployController struct {
	workClient                workv1client.Interface
	addonClient               addonv1alpha1client.Interface
	kubeClient                kubernetes.Interface
	managedClusterLister      clusterlister.ManagedClusterLister
	managedClusterAddonLister addonlisterv1alpha1.ManagedClusterAddOnLister
	workLister                worklister.ManifestWorkLister
	config                    *RegistrationConfig
	agentAddon                addoninterface.AgentAddonWithRegistration
	addonName                 string
}

type RegistrationConfig struct {
	AddonName             string
	AddonInstallNamespace string
	BootstrapSecret       string
	SignerName            string
	ClusterName           string
	KubeConfig            string
}

func NewRegistrationAgentDeployController(
	workClient workv1client.Interface,
	addonClient addonv1alpha1client.Interface,
	kubeClient kubernetes.Interface,
	clusterInformers clusterinformers.ManagedClusterInformer,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	workInformers workinformers.ManifestWorkInformer,
	config *RegistrationConfig,
	agentAddon addoninterface.AgentAddonWithRegistration,
	addonName string,
	recorder events.Recorder,
) factory.Controller {
	c := &registrationAgentDeployController{
		workClient:                workClient,
		addonClient:               addonClient,
		kubeClient:                kubeClient,
		managedClusterLister:      clusterInformers.Lister(),
		managedClusterAddonLister: addonInformers.Lister(),
		workLister:                workInformers.Lister(),
		agentAddon:                agentAddon,
		config:                    config,
		addonName:                 addonName,
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
		},
		addonInformers.Informer()).
		WithInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			}, clusterInformers.Informer()).
		WithSync(c.sync)
	return ctrlFactory.ToController(fmt.Sprintf("%s-addon-registration-controller", addonName), recorder)
}

func (c *registrationAgentDeployController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	clusterName := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling registration agent configuration %q", clusterName)

	// Get ManagedClusterAddon
	managedClusterAddon, err := c.managedClusterAddonLister.ManagedClusterAddOns(clusterName).Get(c.config.AddonName)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	c.config.ClusterName = clusterName
	managedClusterAddon = managedClusterAddon.DeepCopy()
	owner := metav1.NewControllerRef(managedClusterAddon, addonapiv1alpha1.GroupVersion.WithKind("ManagedClusterAddon"))

	// Get ManagedCluster
	cluster, err := c.managedClusterLister.Get(clusterName)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if managedClusterAddon.DeletionTimestamp.IsZero() {
		hasFinalizer := false
		for i := range managedClusterAddon.Finalizers {
			if managedClusterAddon.Finalizers[i] == registrationFinalizer {
				hasFinalizer = true
				break
			}
		}
		if !hasFinalizer {
			managedClusterAddon.Finalizers = append(managedClusterAddon.Finalizers, registrationFinalizer)
			_, err := c.addonClient.AddonV1alpha1().ManagedClusterAddOns(clusterName).Update(ctx, managedClusterAddon, metav1.UpdateOptions{})
			return err
		}
	}

	// Spoke cluster is deleting, we remove its related resources
	if !managedClusterAddon.DeletionTimestamp.IsZero() {
		if err := c.removeRegistrationResources(ctx, cluster); err != nil {
			return err
		}
		return c.removeRegistrationFinalizer(ctx, managedClusterAddon)
	}

	// Generate Bootstrap kubeconfig
	kubeConfigData, err := c.agentAddon.AgentBootstrapKubeConfig(cluster)
	if err != nil {
		return err
	}

	if len(kubeConfigData) > 0 {
		c.config.KubeConfig = base64.StdEncoding.EncodeToString(kubeConfigData)
		c.config.BootstrapSecret = fmt.Sprintf("%s-bootstrap-kubeconfig", c.config.AddonName)

		work := &workapiv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("addon-%s-registration-agent", c.config.AddonName),
				Namespace:       clusterName,
				OwnerReferences: []metav1.OwnerReference{*owner},
			},
			Spec: workapiv1.ManifestWorkSpec{
				Workload: workapiv1.ManifestsTemplate{
					Manifests: []workapiv1.Manifest{},
				},
			},
		}

		// Put registraton agent manifests into work
		for _, file := range registrationAgentFiles {
			rawData := assets.MustCreateAssetFromTemplate(file, bindata.MustAsset(filepath.Join("", file)), c.config).Data
			rawJSON, err := yaml.YAMLToJSON(rawData)
			if err != nil {
				return err
			}
			work.Spec.Workload.Manifests = append(work.Spec.Workload.Manifests, workapiv1.Manifest{
				RawExtension: runtime.RawExtension{Raw: rawJSON},
			})
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
	}

	// apply config/acls on hub for registration agent
	resourceResults := resourceapply.ApplyDirectly(
		resourceapply.NewKubeClientHolder(c.kubeClient),
		syncCtx.Recorder(),
		func(name string) ([]byte, error) {
			return assets.MustCreateAssetFromTemplate(name, bindata.MustAsset(filepath.Join("", name)), c.config).Data, nil
		},
		registrationoHubFiles...,
	)
	errs := []error{}
	for _, result := range resourceResults {
		if result.Error != nil {
			errs = append(errs, fmt.Errorf("%q (%T): %v", result.File, result.Type, result.Error))
		}
	}

	// Apply rbac on hub
	role, rolebinding := c.getRbac(cluster)
	if role != nil {
		_, _, err = resourceapply.ApplyRole(c.kubeClient.RbacV1(), syncCtx.Recorder(), role)
		if err != nil {
			return err
		}
	}

	if rolebinding != nil {
		_, _, err = resourceapply.ApplyRoleBinding(c.kubeClient.RbacV1(), syncCtx.Recorder(), rolebinding)
		if err != nil {
			return err
		}

	}
	return operatorhelpers.NewMultiLineAggregate(errs)
}

func (c *registrationAgentDeployController) removeRegistrationResources(ctx context.Context, cluster *clusterv1.ManagedCluster) error {
	registratonClusterRoleName := fmt.Sprintf("open-cluster-management:managedcluster:%s:addon:%s", cluster.Name, c.addonName)
	err := c.kubeClient.RbacV1().ClusterRoles().Delete(ctx, registratonClusterRoleName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	err = c.kubeClient.RbacV1().ClusterRoleBindings().Delete(ctx, registratonClusterRoleName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	err = c.kubeClient.CoreV1().ConfigMaps(cluster.Name).Delete(ctx, c.addonName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	// remove rbacs
	role, rolebinding := c.getRbac(cluster)
	if role != nil {
		err := c.kubeClient.RbacV1().Roles(role.Namespace).Delete(ctx, role.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	if rolebinding != nil {
		err := c.kubeClient.RbacV1().RoleBindings(rolebinding.Namespace).Delete(ctx, rolebinding.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	return err
}

func (c *registrationAgentDeployController) removeRegistrationFinalizer(ctx context.Context, addon *addonapiv1alpha1.ManagedClusterAddOn) error {
	copiedFinalizers := []string{}
	for i := range addon.Finalizers {
		if addon.Finalizers[i] == registrationFinalizer {
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

func (c *registrationAgentDeployController) getRbac(cluster *clusterv1.ManagedCluster) (*rbacv1.Role, *rbacv1.RoleBinding) {
	group := helpers.AgentGroup(cluster.Name, c.addonName)
	return c.agentAddon.AgentHubRBAC(cluster, group)
}
