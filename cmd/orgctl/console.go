package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// consoleDeployment is the console's Deployment in the control-plane namespace.
const consoleDeployment = "steadmesh-console"

// openConsole forwards a local port to the Steadmesh Console with kubectl
// until interrupted. The console is optional: it exists only when the
// platform stage sets enable_console.
func openConsole(ctx context.Context, c client.Client, kubeconfig, kctx, namespace string, port int) error {
	var d appsv1.Deployment
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: consoleDeployment}, &d)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("console not enabled; set enable_console = true in the platform stage and apply")
	}
	if err != nil {
		return err
	}
	if d.Status.AvailableReplicas == 0 {
		return fmt.Errorf("console deployment %s/%s has no available replicas", namespace, consoleDeployment)
	}
	args := []string{"--context", kctx, "-n", namespace, "port-forward", "deploy/" + consoleDeployment, fmt.Sprintf("%d:8090", port)}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	fmt.Printf("Steadmesh Console: http://127.0.0.1:%d  (Ctrl-C to stop)\n", port)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("kubectl port-forward: %w", err)
	}
	return nil
}
