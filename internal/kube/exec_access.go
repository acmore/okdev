package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ConnectionIdentity binds a reusable connection to the effective kubeconfig,
// including file-backed credentials. Only its digest leaves this method.
func (c *Client) ConnectionIdentity() (string, error) {
	overrides := &clientcmd.ConfigOverrides{CurrentContext: c.Context}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), overrides)
	raw, err := loader.RawConfig()
	if err != nil {
		return "", err
	}
	if c.Context != "" {
		raw.CurrentContext = c.Context
	}
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return "", err
	}
	if err := clientcmdapi.FlattenConfig(&raw); err != nil {
		return "", err
	}
	for _, auth := range raw.AuthInfos {
		if auth.TokenFile != "" {
			token, err := os.ReadFile(auth.TokenFile)
			if err != nil {
				return "", err
			}
			auth.Token = string(token)
		}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// AuthorizeSSHExec is repeated even when SSH can reuse an authenticated master.
func (c *Client) AuthorizeSSHExec(ctx context.Context, namespace, pod string) error {
	client, _, err := c.clientset()
	if err != nil {
		return err
	}
	for _, subresource := range []string{"exec", "portforward"} {
		// WebSocket uses GET; the SPDY fallback uses POST. Recheck both.
		for _, verb := range []string{"get", "create"} {
			review, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: namespace, Verb: verb, Resource: "pods", Subresource: subresource, Name: pod,
				}},
			}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("authorize %s pods/%s: %w", verb, subresource, err)
			}
			if !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
				return fmt.Errorf("SSH exec requires verified %s access to pods/%s on %s/%s: %s %s", verb, subresource, namespace, pod, review.Status.Reason, review.Status.EvaluationError)
			}
		}
	}
	return nil
}
