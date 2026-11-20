package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type runScriptInput struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Interpreter string `json:"interpreter"`
	Args        string `json:"args"`
	TimeoutMS   int    `json:"timeout_ms"`
}

func shellWord(word string) string {
	if runtime.GOOS == "windows" {
		return "'" + strings.ReplaceAll(word, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(word, "'", "'\"'\"'") + "'"
}

func resolveScript(raw json.RawMessage) (runScriptInput, string, error) {
	var in runScriptInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, "", err
	}
	if !filepath.IsLocal(in.Path) || in.Path == "." || strings.ContainsAny(in.Path, "\x00\r\n") {
		return in, "", fmt.Errorf("path 必须为工作目录内的相对文件路径")
	}
	in.Path = filepath.Clean(in.Path)
	if in.Content == "" {
		return in, "", fmt.Errorf("content 不能为空")
	}
	if in.Interpreter == "" {
		switch strings.ToLower(filepath.Ext(in.Path)) {
		case ".py":
			in.Interpreter = "python3"
			if runtime.GOOS == "windows" {
				in.Interpreter = "python"
			}
		case ".js", ".mjs":
			in.Interpreter = "node"
		case ".rb":
			in.Interpreter = "ruby"
		case ".ps1":
			in.Interpreter = "pwsh"
			if runtime.GOOS == "windows" {
				in.Interpreter = "powershell"
			}
		default:
			in.Interpreter = "bash"
		}
	}
	if strings.ContainsAny(in.Interpreter, "\x00\r\n") {
		return in, "", fmt.Errorf("invalid interpreter")
	}
	if in.TimeoutMS <= 0 {
		in.TimeoutMS = 120000
	}
	if in.TimeoutMS > 600000 {
		in.TimeoutMS = 600000
	}
	prefix := ""
	if runtime.GOOS == "windows" {
		prefix = "& "
	}
	command := prefix + shellWord(in.Interpreter)
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(in.Interpreter)), ".exe")
	if base == "powershell" || base == "pwsh" {
		command += " -NoProfile -NonInteractive -File"
	}
	command += " " + shellWord("."+string(filepath.Separator)+in.Path)
	if in.Args != "" {
		command += " " + in.Args
	}
	return in, command, nil
}

type runScriptTool struct{ actool.CoreTool }

func (r *runScriptTool) ReviewInput(raw json.RawMessage) (json.RawMessage, bool, error) {
	in, command, err := resolveScript(raw)
	if err != nil {
		return nil, true, err
	}
	description, err := json.Marshal(map[string]any{"command": command, "path": in.Path, "content": in.Content, "interpreter": in.Interpreter, "timeout_ms": in.TimeoutMS})
	return description, true, err
}

func NewRunScriptTool() actool.CoreTool {
	return &runScriptTool{actool.Build(actool.Spec{
		Name: "RunScript", Description: "在当前工作目录写入脚本并前台执行，原始输出留档。默认按后缀选择解释器，支持 args 和 timeout_ms。",
		Schema: obj(map[string]any{"path": str("相对脚本路径"), "content": str("完整脚本内容"), "interpreter": str("可选解释器可执行文件"), "args": str("可选参数串，按当前系统 shell 语法引用"), "timeout_ms": intp("超时毫秒，默认120000，最多600000")}, "path", "content"),
		Permissions: func(ctx context.Context, raw json.RawMessage, pc permission.Context) permission.Decision {
			in, command, err := resolveScript(raw)
			if err != nil {
				return permission.Denied(err.Error())
			}
			for _, text := range []string{command, in.Content} {
				body, _ := json.Marshal(map[string]string{"command": text})
				if decision := actool.NewBash().CheckPermissions(ctx, body, pc); decision.Behavior == permission.Deny {
					return decision
				}
			}
			return permission.AskUser("write and execute script: " + command)
		},
		Run: runScript,
	})}
}

func runScript(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	in, command, err := resolveScript(raw)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if tc == nil || tc.WorkingDir == "" {
		return actool.Errorf("RunScript 需要工作目录"), nil
	}
	if ctx.Err() != nil {
		return actool.Errorf("执行已取消"), nil
	}
	root, err := os.OpenRoot(tc.WorkingDir)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	defer root.Close()
	if info, err := root.Stat(in.Path); err == nil && !info.Mode().IsRegular() {
		return actool.Errorf("脚本路径不是普通文件"), nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return actool.Errorf(err.Error()), nil
	}
	if err := root.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if err := root.WriteFile(in.Path, []byte(in.Content), 0o700); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if ctx.Err() != nil {
		return actool.Errorf("脚本已保存，执行已取消"), nil
	}
	if tc.Tasks == nil {
		body, _ := json.Marshal(map[string]any{"command": command, "timeout_ms": in.TimeoutMS})
		return actool.NewBash().Call(ctx, body, tc)
	}
	task, finished, output, runErr := tc.Tasks.RunForeground(ctx, actool.SpawnSpec{Command: command, Kind: actool.KindBash, WorkingDir: tc.WorkingDir, Description: "RunScript " + in.Path, Env: tc.Env, ExecutionContext: tc.ExecutionContext}, time.Duration(in.TimeoutMS)*time.Millisecond)
	if task == nil {
		return actool.Errorf("脚本已保存，启动失败：" + fmt.Sprint(runErr)), nil
	}
	if !finished {
		tc.Tasks.Kill(task.ID)
		select {
		case <-task.Done():
		case <-time.After(5 * time.Second):
			return actool.Errorf("脚本进程的终止尚未确认，请核验执行记录"), nil
		}
		output, _ = tc.Tasks.Output(task.ID, 0)
		if runErr == nil {
			runErr = fmt.Errorf("script timed out after %dms", in.TimeoutMS)
		}
	}
	info, _ := tc.Tasks.Get(task.ID)
	tc.Tasks.Forget(task.ID)
	output = actool.Capture(tc, output)
	if runErr != nil {
		return actool.Errorf(output + "\n" + runErr.Error()), nil
	}
	if info.Status == actool.TaskFailed {
		return actool.Errorf(fmt.Sprintf("%s\n[exit code %d]", output, info.ExitCode)), nil
	}
	return actool.Text(output), nil
}
