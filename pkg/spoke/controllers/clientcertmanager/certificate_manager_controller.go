package clientcertmanager

import (
	"context"
	"fmt"
	"path"
	"time"

	addonapiv1alpha1 "github.com/open-cluster-management/api/addon/v1alpha1"
	addonv1alpha1client "github.com/open-cluster-management/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	certificates "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// spokeAgentNameLength is the length of the spoke agent name which is generated automatically
	spokeAgentNameLength = 5
	ClusterNameFile      = "cluster-name"
	AgentNameFile        = "agent-name"
)

type certificateManagerController struct {
	componentNamespace   string
	clusterName          string
	baseHubKubeconfigDir string
	addonClient          addonv1alpha1client.Interface
	hubClientConfig      *restclient.Config
	kubeClient           kubernetes.Interface
	addonLister          addonlisterv1alpha1.ManagedClusterAddOnLister
	configMapLister      corev1lister.ConfigMapLister
	secretInformer       corev1informers.SecretInformer
	hubKubeClient        kubernetes.Interface
	csrControllers       map[string]*certificateManager
}

type certificateManager struct {
	rotateController         *ClientCertForHubController
	stopRotate               context.CancelFunc
	kubeClient               kubernetes.Interface
	secretInformer           corev1informers.SecretInformer
	bootstrapHubKubeClient   kubernetes.Interface
	bootstrapHubClientConfig *restclient.Config
	clusterName              string
	agentName                string
	hubKubeConfigName        string
	hubKubeConfigNameSpace   string
	signer                   string
	addonName                string
	hubKubeconfigDir         string
}

const registrationFinalizer = "addonregistration.open-cluster-management.io"

func NewCertificateManagetController(
	addonClient addonv1alpha1client.Interface,
	kubeClient kubernetes.Interface,
	hubClientConfig *restclient.Config,
	hubKubeClient kubernetes.Interface,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	hubConfigMapInformer corev1informers.ConfigMapInformer,
	secretInformer corev1informers.SecretInformer,
	componentNamespace string,
	clusterName string,
	recorder events.Recorder,
) factory.Controller {
	c := &certificateManagerController{
		componentNamespace: componentNamespace,
		clusterName:        clusterName,
		addonClient:        addonClient,
		kubeClient:         kubeClient,
		hubClientConfig:    hubClientConfig,
		hubKubeClient:      hubKubeClient,
		secretInformer:     secretInformer,
		addonLister:        addonInformers.Lister(),
		configMapLister:    hubConfigMapInformer.Lister(),
		csrControllers:     map[string]*certificateManager{},
	}

	return factory.New().
		WithInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			},
			hubConfigMapInformer.Informer(), addonInformers.Informer()).
		WithSync(c.sync).
		ToController("ClientCertManagerController", recorder)
}

func (c *certificateManagerController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	addonName := syncCtx.QueueKey()
	klog.V(4).Infof("Reconciling addon deploy %q", addonName)

	addon, err := c.addonLister.ManagedClusterAddOns(c.clusterName).Get(addonName)
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}

	cm, err := c.configMapLister.ConfigMaps(c.clusterName).Get(addonName)
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}

	// read configmap data and start cert controller
	if cm.Data["enable_registration"] == "false" {
		return nil
	}

	addon = addon.DeepCopy()
	if addon.DeletionTimestamp.IsZero() {
		hasFinalizer := false
		for i := range addon.Finalizers {
			if addon.Finalizers[i] == registrationFinalizer {
				hasFinalizer = true
				break
			}
		}
		if !hasFinalizer {
			addon.Finalizers = append(addon.Finalizers, registrationFinalizer)
			_, err := c.addonClient.AddonV1alpha1().ManagedClusterAddOns(c.clusterName).Update(ctx, addon, metav1.UpdateOptions{})
			return err
		}
	}

	// addon is deleting, stop registration
	if !addon.DeletionTimestamp.IsZero() {
		certManager := c.csrControllers[addonName]
		if certManager != nil {
			certManager.stopRotate()
		}
		delete(c.csrControllers, addonName)
		return c.removeAddonFinalizer(ctx, addon)
	}

	signer, bootstrapSecret, hubKubeConfigSecretName, hubKubeConfigSecretNameSpace := readConfigFromConfigMap(cm)
	bootstrapHubKubeClient := c.hubKubeClient
	bootstrapHubClientConfig := c.hubClientConfig
	if bootstrapSecret != "" {

	}
	certManager, err := c.newCertificateManager(
		hubKubeConfigSecretName,
		hubKubeConfigSecretNameSpace,
		addonName,
		signer,
		c.baseHubKubeconfigDir,
		bootstrapHubKubeClient,
		bootstrapHubClientConfig,
		c.secretInformer,
		c.kubeClient,
	)
	if err != nil {
		return err
	}

	err = certManager.start(ctx, syncCtx.Recorder())
	if err != nil {
		return err
	}

	c.csrControllers[addonName] = certManager
	return nil
}

func (c *certificateManagerController) newCertificateManager(
	hubKubeConfigSecretName, hubKubeConfigSecretNameSpace, addonName, signer string,
	baseHubKubeconfigDir string,
	bootstrapHubKubeClient kubernetes.Interface,
	bootstrapHubClientConfig *restclient.Config,
	secretInformer corev1informers.SecretInformer,
	kubeClient kubernetes.Interface,
) (*certificateManager, error) {
	bootstrapHubKubeClient, err := kubernetes.NewForConfig(bootstrapHubClientConfig)
	if err != nil {
		return nil, err
	}
	cm := &certificateManager{
		hubKubeConfigName:        hubKubeConfigSecretName,
		hubKubeConfigNameSpace:   hubKubeConfigSecretNameSpace,
		addonName:                addonName,
		signer:                   signer,
		bootstrapHubKubeClient:   bootstrapHubKubeClient,
		bootstrapHubClientConfig: bootstrapHubClientConfig,
		secretInformer:           secretInformer,
		kubeClient:               kubeClient,
		hubKubeconfigDir:         fmt.Sprintf("%s/%s", baseHubKubeconfigDir, addonName),
	}

	secret, err := cm.kubeClient.CoreV1().Secrets(cm.hubKubeConfigNameSpace).Get(context.TODO(), cm.hubKubeConfigName, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		cm.agentName = generateAgentName()
		return cm, nil
	case err != nil:
		return nil, err
	}

	if agentName := secret.Data[AgentNameFile]; agentName != nil {
		cm.agentName = string(agentName)
	} else {
		cm.agentName = generateAgentName()
	}

	return cm, nil
}

func (c *certificateManagerController) removeAddonFinalizer(ctx context.Context, addon *addonapiv1alpha1.ManagedClusterAddOn) error {
	copiedFinalizers := []string{}
	for i := range addon.Finalizers {
		if addon.Finalizers[i] == registrationFinalizer {
			continue
		}
		copiedFinalizers = append(copiedFinalizers, addon.Finalizers[i])
	}

	if len(addon.Finalizers) != len(copiedFinalizers) {
		addon.Finalizers = copiedFinalizers
		_, err := c.addonClient.AddonV1alpha1().ManagedClusterAddOns(c.clusterName).Update(ctx, addon, metav1.UpdateOptions{})
		return err
	}

	return nil
}

func (cm *certificateManager) hasValidHubClientConfig() (bool, error) {
	secret, err := cm.kubeClient.CoreV1().Secrets(cm.hubKubeConfigNameSpace).Get(context.TODO(), cm.hubKubeConfigName, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}

	if secret.Data["kubeconfig"] == nil {
		klog.V(4).Infof("kubeconfig not found")
		return false, nil
	}

	if secret.Data["tls.key"] == nil {
		klog.V(4).Infof("kubeconfig not found")
		return false, nil
	}

	certData := secret.Data["tls.crt"]
	if certData == nil {
		klog.V(4).Infof("kubeconfig not found")
		return false, nil
	}

	agentNameData := secret.Data[AgentNameFile]
	if agentNameData == nil {
		klog.V(4).Infof("agent name not found")
		return false, nil
	}
	agentName := string(agentNameData)

	// check if the tls certificate is issued for the current cluster/agent
	clusterName, agentName, err := GetClusterAgentNamesFromCertificate(certData)
	if err != nil {
		return false, nil
	}
	if clusterName != cm.clusterName || string(agentName) != cm.agentName {
		klog.V(4).Infof("Certificate is issued for agent %q instead of %q", fmt.Sprintf("%s:%s", clusterName, agentName),
			fmt.Sprintf("%s:%s", cm.clusterName, cm.agentName))
		return false, nil
	}

	return IsCertificateValid(certData)
}

func (cm *certificateManager) start(ctx context.Context, recorder events.Recorder) error {
	ok, err := cm.hasValidHubClientConfig()
	if err != nil {
		return err
	}

	certCtx, stopRotate := context.WithCancel(ctx)
	cm.stopRotate = stopRotate
	hubKubeconfigSecretController := NewHubKubeconfigSecretController(
		cm.hubKubeconfigDir, cm.hubKubeConfigNameSpace, cm.hubKubeConfigName,
		cm.kubeClient.CoreV1(),
		cm.secretInformer,
		recorder,
	)
	go hubKubeconfigSecretController.Run(certCtx, 1)

	if !ok {
		// create a ClientCertForHubController for spoke agent bootstrap
		bootstrapInformerFactory := informers.NewSharedInformerFactory(cm.bootstrapHubKubeClient, 10*time.Minute)

		clientCertForHubController := NewClientCertForHubController(
			cm.clusterName, cm.agentName, cm.addonName, cm.signer, cm.hubKubeConfigNameSpace, cm.hubKubeConfigName,
			restclient.AnonymousClientConfig(cm.bootstrapHubClientConfig),
			cm.kubeClient.CoreV1(),
			cm.bootstrapHubKubeClient.CertificatesV1().CertificateSigningRequests(),
			bootstrapInformerFactory.Certificates().V1().CertificateSigningRequests(),
			cm.secretInformer,
			recorder,
			"BootstrapClientCertForHubController",
		)

		bootstrapCtx, stopBootstrap := context.WithCancel(ctx)

		go bootstrapInformerFactory.Start(bootstrapCtx.Done())
		go clientCertForHubController.Run(bootstrapCtx, 1)

		// wait for the hub client config is ready.
		klog.Info("Waiting for hub client config and managed cluster to be ready")
		if err := wait.PollImmediateInfinite(1*time.Second, cm.hasValidHubClientConfig); err != nil {
			// TODO need run the bootstrap CSR forever to re-establish the client-cert if it is ever lost.
			stopBootstrap()
			return err
		}

		// stop the clientCertForHubController for bootstrap once the hub client config is ready
		stopBootstrap()
	}

	// create hub clients and shared informer factories from hub kube config
	hubClientConfig, err := clientcmd.BuildConfigFromFlags("", path.Join(cm.hubKubeconfigDir, KubeconfigFile))
	if err != nil {
		return err
	}

	hubKubeClient, err := kubernetes.NewForConfig(hubClientConfig)
	if err != nil {
		return err
	}

	hubKubeInformerFactory := informers.NewSharedInformerFactory(hubKubeClient, 10*time.Minute)

	// create another ClientCertForHubController for client certificate rotation
	clientCertForHubController := NewClientCertForHubController(
		cm.clusterName, cm.agentName, cm.addonName, cm.signer, cm.hubKubeConfigNameSpace, cm.hubKubeConfigName,
		restclient.AnonymousClientConfig(hubClientConfig),
		cm.kubeClient.CoreV1(),
		hubKubeClient.CertificatesV1().CertificateSigningRequests(),
		hubKubeInformerFactory.Certificates().V1().CertificateSigningRequests(),
		cm.secretInformer,
		recorder,
		"ClientCertForHubController",
	)

	go hubKubeInformerFactory.Start(certCtx.Done())
	go clientCertForHubController.Run(certCtx, 1)
	return nil
}

// generateAgentName generates a random name for spoke cluster agent
func generateAgentName() string {
	return utilrand.String(spokeAgentNameLength)
}

func readConfigFromConfigMap(cm *corev1.ConfigMap) (signer, bootstrapSecret, hubKubeConfigSecretName, hubKubeConfigSecretNameSpace string) {
	signer = certificates.KubeAPIServerClientSignerName
	if cm.Data["signer"] != "" {
		signer = cm.Data["signer"]
	}
	bootstrapSecret = cm.Data["bootstrapSecret"]
	hubKubeConfigSecretName = fmt.Sprintf("%s-hub-kubeconfig", cm.Name)
	hubKubeConfigSecretNameSpace = cm.Data["installNamespace"]
	return signer, bootstrapSecret, hubKubeConfigSecretName, hubKubeConfigSecretNameSpace
}
