// Package e2e holds end-to-end tests that build and run the real
// llm-scheduler binary as a subprocess, against an in-process fake fleet
// console (fakefleet) and fakellama standing in for llama-server-rpc and
// nvidia-smi. The package has no non-test code.
package e2e
