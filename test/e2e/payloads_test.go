//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/chromedp/chromedp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const payloadSecretName = "audit-s3"

// forwardToPod forwards a local port to a port of a pod and returns the
// local address.
func forwardToPod(t *testing.T, admin *kubernetes.Clientset, namespace, pod string, port int) string {
	t.Helper()

	roundTripper, upgrader, err := spdy.RoundTripperFor(adminConfig(t))
	require.NoError(t, err)

	target := admin.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, target)

	stop, ready := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	require.NoError(t, err)

	errs := make(chan error, 1)

	go func() { errs <- forwarder.ForwardPorts() }()

	t.Cleanup(func() { close(stop) })

	select {
	case <-ready:
	case err := <-errs:
		require.NoError(t, err, "port-forward to %s", pod)
	case <-time.After(time.Minute):
		require.FailNow(t, "port-forward to "+pod+" is not ready")
	}

	ports, err := forwarder.GetPorts()
	require.NoError(t, err)
	require.Len(t, ports, 1)

	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
}

// clusterS3 returns a client for the e2e S3 server, the namespace of its
// Secret and the credentials in it.
func clusterS3(t *testing.T, admin *kubernetes.Clientset) (*s3.Client, string, string) {
	t.Helper()

	endpoint := strings.TrimPrefix(getenv("TOOLGATE_E2E_S3_ENDPOINT"), "http://")
	if endpoint == "" {
		t.Skip("TOOLGATE_E2E_S3_ENDPOINT is not set (run through scripts/e2e/run.sh)")
	}

	// The endpoint is s3.<namespace>.svc:<port>.
	parts := strings.Split(endpoint, ".")
	require.GreaterOrEqual(t, len(parts), 2)
	namespace := parts[1]

	secret, err := admin.CoreV1().Secrets(namespace).Get(t.Context(), "dependencies", metav1.GetOptions{})
	require.NoError(t, err)

	pods, err := admin.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{LabelSelector: "app=s3"})
	require.NoError(t, err)
	require.NotEmpty(t, pods.Items)

	accessKey, secretKey := string(secret.Data["s3-access-key"]), string(secret.Data["s3-secret-key"])

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(forwardToPod(t, admin, namespace, pods.Items[0].Name, 7070)),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		UsePathStyle: true,
	})

	return client, accessKey, secretKey
}

func getenv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// objectCount is the number of payload objects in the bucket.
func objectCount(t *testing.T, client *s3.Client, bucket string) int {
	t.Helper()

	output, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	return len(output.Contents)
}

// auditEventLinks lists the type and the page path of each audit event of
// server that the signed-in browser shows.
func auditEventLinks(t *testing.T, browser context.Context, publicURL, server string) map[string]string {
	t.Helper()

	query := url.Values{"server": {server}}

	_, err := chromedp.Run(browser, chromedp.Navigate(publicURL+"/ui/audit?"+query.Encode()))
	require.NoError(t, err)

	rows, err := chromedp.Run(browser, chromedp.Evaluate[[]map[string]string](
		`Array.from(document.querySelectorAll('#events tbody tr')).map(row => ({type: row.cells[1].innerText, href: row.querySelector('a').getAttribute('href')}))`))
	require.NoError(t, err)

	links := map[string]string{}
	for _, row := range rows {
		if _, seen := links[row["type"]]; !seen {
			links[row["type"]] = row["href"] // newest first
		}
	}

	return links
}

func TestAC2_19AuditPayloads(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)
	s3Client, accessKey, secretKey := clusterS3(t, admin)

	name := createTemplate(t, kube, "e2e-payload", "per-user", testServerResources(t, image, 0, nil))
	requireTemplateValid(t, kube, name)

	bucket := "audit-" + strings.ToLower(rand.String(8))
	_, err := s3Client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	_, err = admin.CoreV1().Secrets(releaseNamespace()).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: payloadSecretName},
		StringData: map[string]string{"accessKeyID": accessKey, "secretAccessKey": secretKey},
	}, metav1.CreateOptions{})
	if err != nil {
		require.NoError(t, admin.CoreV1().Secrets(releaseNamespace()).Delete(t.Context(), payloadSecretName, metav1.DeleteOptions{}))
		_, err = admin.CoreV1().Secrets(releaseNamespace()).Create(t.Context(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: payloadSecretName},
			StringData: map[string]string{"accessKeyID": accessKey, "secretAccessKey": secretKey},
		}, metav1.CreateOptions{})
	}

	require.NoError(t, err)

	t.Cleanup(func() {
		_ = admin.CoreV1().Secrets(releaseNamespace()).Delete(context.WithoutCancel(t.Context()), payloadSecretName, metav1.DeleteOptions{})
	})

	upgrade := func(t *testing.T, payloads bool) {
		t.Helper()

		helmCommand(t, "upgrade", "toolgate", chartDir, "--namespace", releaseNamespace(), "--reuse-values",
			"--set", fmt.Sprintf("config.audit.payloads=%t", payloads),
			"--set", "config.audit.s3.endpoint="+getenv("TOOLGATE_E2E_S3_ENDPOINT"),
			"--set", "config.audit.s3.region=us-east-1",
			"--set", "config.audit.s3.bucket="+bucket,
			"--set", "config.audit.s3.pathStyle=true",
			"--set", "config.audit.s3.existingSecret="+payloadSecretName,
			"--wait", "--timeout", "5m")
	}

	// Leave the release as it was found.
	t.Cleanup(func() {
		helmCommand(t, "upgrade", "toolgate", chartDir, "--namespace", releaseNamespace(), "--reuse-values",
			"--set", "config.audit.payloads=false", "--set", "config.audit.s3.bucket=",
			"--set", "config.audit.s3.endpoint=", "--set", "config.audit.s3.region=",
			"--set", "config.audit.s3.pathStyle=false", "--set", "config.audit.s3.existingSecret=",
			"--wait", "--timeout", "5m")
	})

	echo := func(t *testing.T, text string) *mcp.CallToolResult {
		t.Helper()

		endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
		alice := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		enable(t, alice, name)

		result, err := alice.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": text}})
		require.NoError(t, err)
		require.False(t, result.IsError, callText(result))

		return result
	}

	t.Run("[AC-2-19] with payloads off neither a small nor a large call is stored", func(t *testing.T) {
		echo(t, "small off")
		echo(t, strings.Repeat("l", audit.InlinePayloadLimit+1024))

		assert.Zero(t, objectCount(t, s3Client, bucket))
	})

	upgrade(t, true)

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	publicURL := webUI(t, endpoint)
	browser := signInToUI(t, publicURL, testutil.OIDCUser{
		Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"developers"},
	})

	view := func(t *testing.T, href string) string {
		t.Helper()

		_, err := chromedp.Run(browser, chromedp.Navigate(publicURL+href))
		require.NoError(t, err)

		text, err := chromedp.Run(browser, chromedp.Evaluate[string](`document.querySelector('main').innerText`))
		require.NoError(t, err)

		return text
	}

	t.Run("[AC-2-19] a small result is inline and viewable from its event", func(t *testing.T) {
		echo(t, "small marker on")

		require.Never(t, func() bool { return objectCount(t, s3Client, bucket) > 0 }, 2*time.Second, time.Second,
			"nothing under 64 KiB goes to S3")

		links := auditEventLinks(t, browser, publicURL, name)
		require.Contains(t, links, "tool.result")
		assert.Contains(t, view(t, links["tool.result"]), "small marker on")
	})

	t.Run("[AC-2-19] a result over 64 KiB is in S3 and viewable from its event", func(t *testing.T) {
		marker := "BIGMARKER" + rand.String(6)
		echo(t, marker+strings.Repeat("l", audit.InlinePayloadLimit+1024))

		assert.Eventually(t, func() bool { return objectCount(t, s3Client, bucket) >= 1 }, 30*time.Second, time.Second)

		links := auditEventLinks(t, browser, publicURL, name)
		require.Contains(t, links, "tool.result")
		assert.Contains(t, view(t, links["tool.result"]), marker)
	})
}
