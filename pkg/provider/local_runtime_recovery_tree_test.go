package provider

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Starts the copied server binary as a router the way an owner launches it, then exits without cleanup
func lr7OwnerRole() {
	router := exec.Command(os.Getenv("ICEBATH_LRT_ROUTER_EXE"), "-test.run=^TestLocalRuntimeProcessHelper$", "-test.v=false", "--",
		"--host", "127.0.0.1", "--port", "59999", "--api-key", "test-only", "--models-dir", os.Getenv("ICEBATH_LRT_MODELS_DIR"))
	router.Env = append(os.Environ(), lr3HelperRoleEnv+"=lr7router")
	if err := router.Start(); err != nil {
		os.Exit(3)
	}
	seconds, _ := strconv.Atoi(os.Getenv("ICEBATH_LRT_OWNER_SECONDS"))
	time.Sleep(time.Duration(seconds) * time.Second)
}
