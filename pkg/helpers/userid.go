package helpers

import "fmt"

func AgentUser(clusterName, agentName, addonName string) string {
	return fmt.Sprintf("%s:agent:%s", AgentGroup(clusterName, addonName), agentName)
}

func AgentGroup(clusterName, addonName string) string {
	return fmt.Sprintf("system:open-cluster-management:cluster:%s:addon:%s", clusterName, addonName)
}

func AgentCSRGenerateName(clusterName, addonName string) string {
	return fmt.Sprintf("addon-%s-%s", addonName, clusterName)
}
