package cocoonset

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
)

const agentContainer = "agent"

var (
	writeIdentity = []string{"sh", "-c", `set -e; umask 022; mkdir -p /run/cocoonbox-identity; chmod 0755 /run/cocoonbox-identity
t=$(mktemp /run/cocoonbox-identity/.identity.XXXXXX); cat > "$t"; chmod 0644 "$t"; mv -f "$t" /run/cocoonbox-identity/identity`}
	writeGWKey = []string{"sh", "-c", `set -e; read -r key; { grep -v '^GW_API_KEY=' /etc/cocoonbox/agent.env; printf 'GW_API_KEY=%s\n' "$key"; } |
/usr/local/lib/cocoonbox/set-agent-env; systemctl restart cocoonbox-agent`}
)

// podExecFunc runs cmd in a pod's container and returns stdout.
type podExecFunc func(ctx context.Context, pod string, cmd []string, stdin []byte) ([]byte, error)

func newPodExec(restCfg *rest.Config, kube kubernetes.Interface, namespace string) podExecFunc {
	return func(ctx context.Context, pod string, cmd []string, stdin []byte) ([]byte, error) {
		req := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: agentContainer, Command: cmd, Stdin: stdin != nil, Stdout: true, Stderr: true,
			}, scheme.ParameterCodec)
		ws, err := remotecommand.NewWebSocketExecutor(restCfg, "GET", req.URL().String())
		if err != nil {
			return nil, fmt.Errorf("websocket executor: %w", err)
		}
		spdy, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
		if err != nil {
			return nil, fmt.Errorf("spdy executor: %w", err)
		}
		exec, err := remotecommand.NewFallbackExecutor(ws, spdy, httpstream.IsUpgradeFailure)
		if err != nil {
			return nil, fmt.Errorf("exec executor: %w", err)
		}
		var stdout, stderr bytes.Buffer
		opts := remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}
		if stdin != nil {
			opts.Stdin = bytes.NewReader(stdin)
		}
		if err := exec.StreamWithContext(ctx, opts); err != nil {
			return nil, fmt.Errorf("exec %s in %s: %w (stderr %q)", cmd[0], pod, err, stderr.String())
		}
		return stdout.Bytes(), nil
	}
}
