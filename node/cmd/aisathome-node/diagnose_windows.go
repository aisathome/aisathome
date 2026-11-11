// SPDX-License-Identifier: Apache-2.0
package main

import (
	"path/filepath"

	"github.com/aisathome/aisathome/node"
)

var osVersionCommand = []string{"cmd", "/c", "ver"}

// serviceCommands: the service, its recovery settings, the processes, the data folder's
// permissions, the Service Control Manager's and application crash events, and the GPU
// timeout (TDR) settings.
func serviceCommands(l node.Layout, exeDir string) []diagCommand {
	dir := filepath.Dir(l.Config)
	return []diagCommand{
		{"sc-query", []string{"sc.exe", "queryex", node.ServiceName}},
		{"sc-qc", []string{"sc.exe", "qc", node.ServiceName}},
		{"sc-qfailure", []string{"sc.exe", "qfailure", node.ServiceName}},
		{"sc-qfailureflag", []string{"sc.exe", "qfailureflag", node.ServiceName}},
		{"tasklist", []string{"tasklist.exe", "/v", "/fi", "imagename eq llama-server.exe"}},
		{"tasklist-agent", []string{"tasklist.exe", "/v", "/fi", "imagename eq aisathome-node.exe"}},
		{"icacls-data", []string{"icacls.exe", dir}},
		{"icacls-files", []string{"icacls.exe", filepath.Join(dir, "*")}},
		{"events-scm", []string{"wevtutil.exe", "qe", "System", "/q:*[System[Provider[@Name='Service Control Manager']]]", "/c:60", "/rd:true", "/f:text"}},
		{"events-crashes", []string{"wevtutil.exe", "qe", "Application", "/q:*[System[Provider[@Name='Application Error' or @Name='Windows Error Reporting']]]", "/c:30", "/rd:true", "/f:text"}},
		{"events-display", []string{"wevtutil.exe", "qe", "System", "/q:*[System[Provider[@Name='Display' or @Name='nvlddmkm']]]", "/c:30", "/rd:true", "/f:text"}},
		{"tdr", []string{"reg.exe", "query", `HKLM\SYSTEM\CurrentControlSet\Control\GraphicsDrivers`}},
		{"engine-version", []string{filepath.Join(exeDir, l.EngineProgram), "--version"}},
	}
}
