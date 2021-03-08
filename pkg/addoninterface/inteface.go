package addoninterface

import (
	clusterv1 "github.com/open-cluster-management/api/cluster/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentAddon defines an interface on manifests of agent deployed on managed cluster and rbac rules of agent on hub
type AgentAddon interface {
	// AgentManifests returns a list of manifest resources to be deployed on the managed cluster for this addon
	AgentManifests(cluster *clusterv1.ManagedCluster, config runtime.Object) ([]runtime.Object, error)
}

type AgentAddonWithRegistration interface {
	AgentAddon
	// AgentHubRBAC returns the role and rolebinding on the hub for an agent on the managed cluster.
	AgentHubRBAC(cluster *clusterv1.ManagedCluster, group string) (*rbacv1.Role, *rbacv1.RoleBinding)

	// AgentBootstrapKubeConfig returns the bootstrap kubeconfig used by the agent to registerto hub.
	AgentBootstrapKubeConfig(cluster *clusterv1.ManagedCluster) ([]byte, error)
}

// CSRApproveCheckFunc check if a csr should be approved
type CSRApproveCheckFunc func(csr *certificatesv1.CertificateSigningRequest) bool
