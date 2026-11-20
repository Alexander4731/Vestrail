package agent

import actool "github.com/Autumn-27/norma/tool"

func plannerToolsWithoutBash(tools []actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name() != "Bash" {
			out = append(out, tool)
		}
	}
	return out
}
