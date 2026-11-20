//go:build !linux

package main

import "github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"

func restartExec(exe string) error { return osx.ErrUnsupported }
