package addonmanager

import (
	"context"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/openshift/library-go/pkg/operator/events"
	csrv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/open-cluster-management/addon-framework/pkg/addoninterface"
	"github.com/open-cluster-management/addon-framework/pkg/addonmanager/agentdeploy"
	"github.com/open-cluster-management/addon-framework/pkg/addonmanager/clustermanagement"
	"github.com/open-cluster-management/addon-framework/pkg/addonmanager/registration"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformers "github.com/open-cluster-management/api/client/addon/informers/externalversions"
	clusterv1client "github.com/open-cluster-management/api/client/cluster/clientset/versioned"
	clusterv1informers "github.com/open-cluster-management/api/client/cluster/informers/externalversions"
	workv1client "github.com/open-cluster-management/api/client/work/clientset/versioned"
	workv1informers "github.com/open-cluster-management/api/client/work/informers/externalversions"
)

// AddonManagerContext defines the context of addon manager.
type AddonManagerContext struct {
	// KubeConfig provides the REST config with no content type (it will default to JSON).
	KubeConfig *rest.Config

	// AddonName is the name of the addon
	AddonName string

	// AddonNamespace is the namespace to deploy addon agent on spoke
	AddonInstallNamespace string

	// ConfigurationGVR is the gvr of the resource for configuring addon
	ConfigurationGVR *schema.GroupVersionResource
}

// AddonManager defines an addon manager on hub to trigger the installation and configuration
// of addon agent on managed cluster.
type AddonManager struct {
	managerContext     *AddonManagerContext
	agentAddon         addoninterface.AgentAddon
	eventRecorder      events.Recorder
	enableRegistration bool
	enableCSRApprove   bool
	registrationConfig *registration.RegistrationConfig
	approveCheckFuncs  []addoninterface.CSRApproveCheckFunc
}

// NewAddonManager returns an addon manager
func NewAddonManager(managerContext *AddonManagerContext, agentAddon addoninterface.AgentAddon) *AddonManager {
	return &AddonManager{
		managerContext:    managerContext,
		agentAddon:        agentAddon,
		approveCheckFuncs: []addoninterface.CSRApproveCheckFunc{},
		registrationConfig: &registration.RegistrationConfig{
			AddonName:             managerContext.AddonName,
			AddonInstallNamespace: managerContext.AddonInstallNamespace,
		},
	}
}

func (a *AddonManager) WithRegistrationEnabled() *AddonManager {
	a.enableRegistration = true
	if a.registrationConfig.SignerName == "" {
		a.registrationConfig.SignerName = csrv1.KubeAPIServerClientSignerName
	}
	return a
}

func (a *AddonManager) WithSigner(signer string) *AddonManager {
	a.registrationConfig.SignerName = signer
	return a
}

func (a *AddonManager) WithEnableCSRApproveFunc(approveFuncs ...addoninterface.CSRApproveCheckFunc) *AddonManager {
	a.enableCSRApprove = true
	a.approveCheckFuncs = append(a.approveCheckFuncs, approveFuncs...)
	return a
}

// Run starts the addon manager
func (a *AddonManager) Run(ctx context.Context) error {
	kubeClient, err := kubernetes.NewForConfig(a.managerContext.KubeConfig)
	if err != nil {
		return err
	}

	addonClient, err := addonv1alpha1client.NewForConfig(a.managerContext.KubeConfig)
	if err != nil {
		return err
	}

	clusterClient, err := clusterv1client.NewForConfig(a.managerContext.KubeConfig)
	if err != nil {
		return err
	}

	workClient, err := workv1client.NewForConfig(a.managerContext.KubeConfig)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(a.managerContext.KubeConfig)
	if err != nil {
		return err
	}

	namespace, err := a.getComponentNamespace()
	if err != nil {
		klog.Warningf("unable to identify the current namespace for events: %v", err)
	}
	controllerRef, err := events.GetControllerReferenceForCurrentPod(kubeClient, namespace, nil)
	if err != nil {
		klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
	}

	eventRecorder := events.NewKubeRecorder(
		kubeClient.CoreV1().Events(namespace), a.managerContext.AddonName, controllerRef)

	addonInformers := addoninformers.NewSharedInformerFactory(addonClient, 10*time.Minute)
	workInformers := workv1informers.NewSharedInformerFactory(workClient, 10*time.Minute)
	clusterInformers := clusterv1informers.NewSharedInformerFactory(clusterClient, 10*time.Minute)
	kubeInfomers := kubeinformers.NewSharedInformerFactory(kubeClient, 10*time.Minute)
	dynamicInformer := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 10*time.Minute)

	deployController := agentdeploy.NewAddonDeployController(
		workClient,
		addonClient,
		kubeClient,
		clusterInformers.Cluster().V1().ManagedClusters(),
		addonInformers.Addon().V1alpha1().ManagedClusterAddOns(),
		workInformers.Work().V1().ManifestWorks(),
		dynamicInformer,
		a.managerContext.ConfigurationGVR,
		a.agentAddon,
		a.managerContext.AddonName,
		eventRecorder,
	)

	clusterManagementController := clustermanagement.NewClusterManagementController(
		addonClient,
		addonInformers.Addon().V1alpha1().ClusterManagementAddOns(),
		a.managerContext.AddonName,
		eventRecorder,
	)

	if a.enableCSRApprove {
		registrationController := registration.NewCSRApprovingController(
			kubeClient,
			kubeInfomers.Certificates().V1().CertificateSigningRequests().Informer(),
			eventRecorder,
			a.registrationConfig.SignerName, a.managerContext.AddonName,
			a.approveCheckFuncs...,
		)
		go registrationController.Run(ctx, 1)
	}

	if a.enableRegistration {
		agentAddonWithRegistration, ok := a.agentAddon.(addoninterface.AgentAddonWithRegistration)
		if !ok {
			return fmt.Errorf("some interface of addon agent with registration is not implemented")
		}
		registrationDeployController := registration.NewRegistrationAgentDeployController(
			workClient,
			addonClient,
			kubeClient,
			clusterInformers.Cluster().V1().ManagedClusters(),
			addonInformers.Addon().V1alpha1().ManagedClusterAddOns(),
			workInformers.Work().V1().ManifestWorks(),
			a.registrationConfig, agentAddonWithRegistration, a.managerContext.AddonName,
			eventRecorder,
		)
		go registrationDeployController.Run(ctx, 1)
	}

	go deployController.Run(ctx, 1)
	go clusterManagementController.Run(ctx, 1)

	go addonInformers.Start(ctx.Done())
	go workInformers.Start(ctx.Done())
	go clusterInformers.Start(ctx.Done())
	go kubeInfomers.Start(ctx.Done())
	go dynamicInformer.Start(ctx.Done())

	<-ctx.Done()
	return nil
}

func (a *AddonManager) getComponentNamespace() (string, error) {
	nsBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "open-cluster-management", err
	}
	return string(nsBytes), nil
}
