package server

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// annotationCopiedFrom says whose database a copied Secret is for.
const annotationCopiedFrom = "forge.dev/copied-from"

// resourceID is the last segment of "resources/<service>/<env>/<name>".
func resourceID(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

// CopyDatabaseSecret gives the part to, in env, the URL of the database of
// the part from: it reads the URL from from's Secret secret and writes it,
// alone, as "uri" into linkSecretName in to's namespace. Both namespaces
// must be Forge's. forge-api may read the one Secret and replace the other
// only because the service chart says so in each namespace (a Role for a
// shared database, and one for a linked one); the URL is never logged.
func (c *Cluster) CopyDatabaseSecret(ctx context.Context, from, to, env, secret string) error {
	for _, svc := range []string{from, to} {
		if err := c.checkServiceNamespace(ctx, svc, env); err != nil {
			return fmt.Errorf("namespace %s-%s: %w", svc, env, err)
		}
	}
	src, err := c.Kube.CoreV1().Secrets(from+"-"+env).Get(ctx, secret, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("forge-api may not read Secret %s in %s-%s: the part must be deployed, with its database shared, by a service chart that knows shared databases", secret, from, env)
	}
	if err != nil {
		return fmt.Errorf("read Secret %s in %s-%s: %w", secret, from, env, err)
	}
	// "fqdn-uri" names the database by its full name. "uri" has the short
	// one, which resolves only in the database's own namespace, so it is
	// never copied.
	url := src.Data["fqdn-uri"]
	if len(url) == 0 {
		return fmt.Errorf("the Secret %s in %s-%s has no fqdn-uri, the URL that works from another namespace", secret, from, env)
	}
	cp := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        linkSecretName,
			Labels:      map[string]string{labelManagedBy: "forge"},
			Annotations: map[string]string{annotationCopiedFrom: from + "-" + env + "/" + secret},
		},
		Data: map[string][]byte{"uri": url},
	}
	secrets := c.Kube.CoreV1().Secrets(to + "-" + env)
	_, err = secrets.Create(ctx, cp, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, cp, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("write Secret %s in %s-%s: %w", linkSecretName, to, env, err)
	}
	return nil
}

// DeleteLinkedDatabase removes the copy CopyDatabaseSecret made in
// <service>-<env>. One that is gone already is fine.
func (c *Cluster) DeleteLinkedDatabase(ctx context.Context, service, env string) error {
	if err := c.checkServiceNamespace(ctx, service, env); err != nil {
		if err == errNoEnv { //nolint:errorlint // the sentinel itself
			return nil
		}
		return err
	}
	err := c.Kube.CoreV1().Secrets(service+"-"+env).Delete(ctx, linkSecretName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
