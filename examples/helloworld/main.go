package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"time"

	goflag "flag"

	"github.com/open-cluster-management/addon-framework/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"

	"github.com/open-cluster-management/addon-framework/pkg/addonmanager"
	clusterv1 "github.com/open-cluster-management/api/cluster/v1"
	certificates "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func main() {
	rand.Seed(time.Now().UTC().UnixNano())

	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	logs.InitLogs()
	defer logs.FlushLogs()

	command := newCommand()
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("addon-controller", version.Get(), runController).
		NewCommand()
	cmd.Use = "controller"
	cmd.Short = "Start the addon controller"

	return cmd
}

func runController(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	addonContext := &addonmanager.AddonManagerContext{
		AddonName:             "helloworld",
		AddonInstallNamespace: "default",
		KubeConfig:            controllerContext.KubeConfig,
	}
	agent := &helloWorldAgent{}
	mgr := addonmanager.NewAddonManager(addonContext, agent).
		WithRegistrationEnabled().
		WithEnableCSRApproveFunc(
			func(csr *certificates.CertificateSigningRequest) bool {
				return true
			},
		)

	mgr.Run(ctx)

	<-ctx.Done()

	return nil
}

type helloWorldAgent struct{}

func (h *helloWorldAgent) AgentManifests(cluster *clusterv1.ManagedCluster, config runtime.Object) ([]runtime.Object, error) {
	return []runtime.Object{
		&corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				Kind:       "ConfigMap",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hello-world",
				Namespace: "default",
			},
		},
	}, nil
}
