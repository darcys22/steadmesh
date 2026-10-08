package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/connectors/terminal"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// platformService is the platform's Service in the control-plane namespace.
const platformService = "steadmesh-platform"

type chatOptions struct {
	kubeconfig, kctx, systemNS string
	user, connection, token    string
}

// chat opens a terminal conversation with the user's representative: lines
// read from stdin go to the representative, and its messages are printed as
// they arrive. The platform is reached through kubectl port-forward.
func chat(ctx context.Context, c client.Client, orgs []v1alpha1.AgentOrganization, o chatOptions) error {
	if o.user == "" {
		return errors.New("--user is required (the human's external user ID on the terminal connection)")
	}
	if len(orgs) != 1 {
		return errors.New("more than one AgentOrganization; choose one with --namespace and --name")
	}
	org := &orgs[0]
	orgID := org.Status.OrganizationID
	if orgID == "" {
		return fmt.Errorf("organisation %s/%s has no organisation ID yet; wait for the controller to sync it", org.Namespace, org.Name)
	}
	connKey, err := terminalConnection(org, o.user, o.connection)
	if err != nil {
		return err
	}
	token := o.token
	if token == "" {
		if token, err = readToken(ctx, c, o.systemNS, org.Spec.Connections[connKey].SecretRef, o.user); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	base, err := portForward(ctx, o)
	if err != nil {
		return err
	}
	cl := &chatClient{base: base + runtimeapi.PathChannels + orgID + "/" + connKey, token: token}

	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		return chatTUI(ctx, cl, o.user, connKey)
	}
	return chatPlain(ctx, cl, o.user, connKey)
}

// chatPlain is the line-oriented chat used when stdin or stdout is not a
// terminal, e.g. when input is piped.
func chatPlain(ctx context.Context, cl *chatClient, user, connKey string) error {
	var mu sync.Mutex
	printf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Printf(format, args...)
	}
	printf("Chatting with %s's representative over %s. Ctrl-D or Ctrl-C to leave.\n", user, connKey)
	go cl.receive(ctx,
		func(ev terminal.Event) { printf("representative> %s\n", ev.Text) },
		func(status string) { printf("! %s\n", status) })
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return nil
		case line, ok := <-lines:
			if !ok {
				fmt.Println()
				return nil
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			if err := cl.send(ctx, line); err != nil {
				printf("! not delivered: %v\n", err)
			}
		}
	}
}

// terminalConnection picks the terminal connection that binds user: the
// named one, or the only one.
func terminalConnection(org *v1alpha1.AgentOrganization, user, want string) (string, error) {
	var found []string
	for _, k := range sortedKeys(org.Spec.ChannelBindings) {
		b := org.Spec.ChannelBindings[k]
		conn, ok := org.Spec.Connections[b.Connection]
		if !ok || conn.Adapter != "terminal" || b.ExternalUserID != user {
			continue
		}
		if want == "" || want == b.Connection {
			found = append(found, b.Connection)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no terminal channel binding for user %q in %s/%s", user, org.Namespace, org.Name)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("user %q is bound on several terminal connections (%s); choose one with --connection", user, strings.Join(found, ", "))
	}
}

// readToken reads the user's token from the connection's Kubernetes Secret.
func readToken(ctx context.Context, c client.Client, ns, ref, user string) (string, error) {
	name, ok := strings.CutPrefix(ref, "k8s:")
	if !ok {
		return "", fmt.Errorf("secret %s is not a Kubernetes Secret; pass the token with --token or STEADMESH_TERMINAL_TOKEN", ref)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		return "", fmt.Errorf("read terminal token from %s/%s: %w", ns, name, err)
	}
	tok := strings.TrimSpace(string(s.Data[user]))
	if tok == "" {
		return "", fmt.Errorf("secret %s/%s has no token for user %q", ns, name, user)
	}
	return tok, nil
}

// portForward forwards a free local port to the platform until ctx ends and
// returns its base URL once the platform answers.
func portForward(ctx context.Context, o chatOptions) (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	args := []string{"--context", o.kctx, "-n", o.systemNS, "port-forward", "svc/" + platformService, fmt.Sprintf("%d:8080", port)}
	if o.kubeconfig != "" {
		args = append([]string{"--kubeconfig", o.kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("kubectl port-forward: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		select {
		case <-exited:
			return "", fmt.Errorf("kubectl port-forward exited: %s", strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if resp, err := http.Get(base + runtimeapi.PathHealthz); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, nil
			}
		}
	}
	return "", errors.New("platform did not answer through kubectl port-forward")
}

// chatClient talks to the terminal adapter.
type chatClient struct {
	base, token string
}

func (c *chatClient) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// send posts one line, retrying with the same id so it is delivered once.
func (c *chatClient) send(ctx context.Context, text string) error {
	body, _ := json.Marshal(terminal.Inbound{ID: uuid.NewString(), Text: text})
	var last error
	for attempt := range 5 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		resp, err := c.request(ctx, http.MethodPost, "/messages", bytes.NewReader(body))
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusAccepted:
			return nil
		case resp.StatusCode >= 500:
			last = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
		default:
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
	}
	return last
}

// receive passes the representative's messages to onEvent until ctx ends,
// reconnecting when the stream drops and reporting that to onStatus.
func (c *chatClient) receive(ctx context.Context, onEvent func(terminal.Event), onStatus func(string)) {
	for ctx.Err() == nil {
		err := c.stream(ctx, onEvent)
		if ctx.Err() != nil {
			return
		}
		onStatus(fmt.Sprintf("connection to representative lost (%v); reconnecting", err))
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *chatClient) stream(ctx context.Context, onEvent func(terminal.Event)) error {
	resp, err := c.request(ctx, http.MethodGet, "/events", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev terminal.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		onEvent(ev)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
