package cmd

import (
	"github.com/open-cluster-management/addon-framework/pkg/hub"
	"github.com/open-cluster-management/addon-framework/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/cobra"
)

func NewManager() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("agent", version.Get(), hub.RunControllerManager).
		NewCommand()
	cmd.Use = "manager"
	cmd.Short = "Start the Addon Manager"
	return cmd
}
