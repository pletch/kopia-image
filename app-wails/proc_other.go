//go:build !windows

package main

import "os/exec"

func hideConsoleWindow(*exec.Cmd) {}

func electronKopiaUIRunning() bool { return false }
