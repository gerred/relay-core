package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	nebulav1 "github.com/puppetlabs/relay-core/pkg/apis/nebula.puppet.com/v1"
	relayv1beta1 "github.com/puppetlabs/relay-core/pkg/apis/relay.sh/v1beta1"
	"github.com/puppetlabs/relay-core/pkg/expr/evaluate"
	"github.com/puppetlabs/relay-core/pkg/model"
	"github.com/puppetlabs/relay-core/pkg/obj"
	"github.com/puppetlabs/relay-core/pkg/util/retry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkflowRunWithTenantVolumeClaim(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	WithConfig(t, ctx, []ConfigOption{
		ConfigWithMetadataAPI,
		ConfigWithTenantReconciler,
		ConfigWithWorkflowRunReconciler,
	}, func(cfg *Config) {
		size, _ := resource.ParseQuantity("100Mi")
		storageClassName := "example-hostpath"
		tenant := &relayv1beta1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cfg.Namespace.GetName(),
				Name:      "tenant-" + uuid.New().String(),
			},
			Spec: relayv1beta1.TenantSpec{
				NamespaceTemplate: relayv1beta1.NamespaceTemplate{
					Metadata: metav1.ObjectMeta{
						Name: cfg.Namespace.GetName(),
					},
				},
				ToolInjection: relayv1beta1.ToolInjection{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.ResourceRequirements{
								Requests: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceStorage: size,
								},
							},
							StorageClassName: &storageClassName,
						},
					},
				},
			},
		}

		CreateAndWaitForTenant(t, ctx, tenant)

		var ns corev1.Namespace
		require.Equal(t, cfg.Namespace.GetName(), tenant.Status.Namespace)
		require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: cfg.Namespace.GetName()}, &ns))

		var job batchv1.Job
		require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + "-volume", Namespace: tenant.Status.Namespace}, &job))

		var pvcw corev1.PersistentVolumeClaim
		require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + "-volume-rwo", Namespace: tenant.Status.Namespace}, &pvcw))

		var pvcr corev1.PersistentVolumeClaim
		require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + "-volume-rox", Namespace: tenant.Status.Namespace}, &pvcr))

		var pv corev1.PersistentVolume
		require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + "-volume-rox"}, &pv))

		// Set a secret and connection for this workflow to look up.
		cfg.Vault.SetSecret(t, "my-tenant-id", "foo", "Hello")
		cfg.Vault.SetConnection(t, "my-domain-id", "aws", "test", map[string]string{
			"accessKeyID":     "AKIA123456789",
			"secretAccessKey": "very-nice-key",
		})

		wr := &nebulav1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cfg.Namespace.GetName(),
				Name:      "my-test-run",
				Annotations: map[string]string{
					model.RelayVaultEngineMountAnnotation:    cfg.Vault.SecretsPath,
					model.RelayVaultConnectionPathAnnotation: "connections/my-domain-id",
					model.RelayVaultSecretPathAnnotation:     "workflows/my-tenant-id",
					model.RelayDomainIDAnnotation:            "my-domain-id",
					model.RelayTenantIDAnnotation:            "my-tenant-id",
				},
			},
			Spec: nebulav1.WorkflowRunSpec{
				Name: "my-workflow-run-1234",
				TenantRef: &corev1.LocalObjectReference{
					Name: tenant.GetName(),
				},
				Workflow: nebulav1.Workflow{
					Parameters: relayv1beta1.NewUnstructuredObject(map[string]interface{}{
						"Hello": "World!",
					}),
					Name: "my-workflow",
					Steps: []*nebulav1.WorkflowStep{
						{
							Name:  "my-test-step",
							Image: "alpine:latest",
							Spec: relayv1beta1.NewUnstructuredObject(map[string]interface{}{
								"secret": map[string]interface{}{
									"$type": "Secret",
									"name":  "foo",
								},
								"connection": map[string]interface{}{
									"$type": "Connection",
									"type":  "aws",
									"name":  "test",
								},
								"param": map[string]interface{}{
									"$type": "Parameter",
									"name":  "Hello",
								},
							}),
							Input: []string{
								"trap : TERM INT",
								"sleep 600 & wait",
							},
						},
					},
				},
			},
		}
		require.NoError(t, e2e.ControllerRuntimeClient.Create(ctx, wr))

		// Wait for step to start. Could use a ListWatcher but meh.
		require.NoError(t, retry.Retry(ctx, 500*time.Millisecond, func() *retry.RetryError {
			if err := e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{
				Namespace: wr.GetNamespace(),
				Name:      wr.GetName(),
			}, wr); err != nil {
				return retry.RetryPermanent(err)
			}

			if wr.Status.Steps["my-test-step"].Status == string(obj.WorkflowRunStatusInProgress) {
				return retry.RetryPermanent(nil)
			}

			return retry.RetryTransient(fmt.Errorf("waiting for step to start"))
		}))

		// Pull the pod and get its IP.
		pod := &corev1.Pod{}
		require.NoError(t, retry.Retry(ctx, 500*time.Millisecond, func() *retry.RetryError {
			pods := &corev1.PodList{}
			if err := e2e.ControllerRuntimeClient.List(ctx, pods, client.InNamespace(wr.GetNamespace()), client.MatchingLabels{
				// TODO: We shouldn't really hardcode this.
				"tekton.dev/task": (&model.Step{Run: model.Run{ID: wr.Spec.Name}, Name: "my-test-step"}).Hash().HexEncoding(),
			}); err != nil {
				return retry.RetryPermanent(err)
			}

			if len(pods.Items) == 0 {
				return retry.RetryTransient(fmt.Errorf("waiting for pod"))
			}

			pod = &pods.Items[0]
			if pod.Status.PodIP == "" {
				return retry.RetryTransient(fmt.Errorf("waiting for pod IP"))
			} else if pod.Status.Phase == corev1.PodPending {
				return retry.RetryTransient(fmt.Errorf("waiting for pod to start"))
			}

			return retry.RetryPermanent(nil)
		}))

		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/spec", cfg.MetadataAPIURL), nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", pod.Status.PodIP)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var result evaluate.JSONResultEnvelope
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		assert.True(t, result.Complete)
		assert.Equal(t, map[string]interface{}{
			"secret": "Hello",
			"connection": map[string]interface{}{
				"accessKeyID":     "AKIA123456789",
				"secretAccessKey": "very-nice-key",
			},
			"param": "World!",
		}, result.Value.Data)

		e2e.ControllerRuntimeClient.Delete(ctx, &job)
		e2e.ControllerRuntimeClient.Delete(ctx, &pvcw)
		e2e.ControllerRuntimeClient.Delete(ctx, &pvcr)
		e2e.ControllerRuntimeClient.Delete(ctx, &pv)
	})
}

// TestWorkflowRun tests that an instance of the controller, when given a run to
// process, correctly sets up a Tekton pipeline and that the resulting pipeline
// should be able to access a metadata API service.
func TestWorkflowRun(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	WithConfig(t, ctx, []ConfigOption{
		ConfigWithMetadataAPI,
		ConfigWithWorkflowRunReconciler,
	}, func(cfg *Config) {
		// Set a secret and connection for this workflow to look up.
		cfg.Vault.SetSecret(t, "my-tenant-id", "foo", "Hello")
		cfg.Vault.SetConnection(t, "my-domain-id", "aws", "test", map[string]string{
			"accessKeyID":     "AKIA123456789",
			"secretAccessKey": "very-nice-key",
		})

		wr := &nebulav1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cfg.Namespace.GetName(),
				Name:      "my-test-run",
				Annotations: map[string]string{
					model.RelayVaultEngineMountAnnotation:    cfg.Vault.SecretsPath,
					model.RelayVaultConnectionPathAnnotation: "connections/my-domain-id",
					model.RelayVaultSecretPathAnnotation:     "workflows/my-tenant-id",
					model.RelayDomainIDAnnotation:            "my-domain-id",
					model.RelayTenantIDAnnotation:            "my-tenant-id",
				},
			},
			Spec: nebulav1.WorkflowRunSpec{
				Name: "my-workflow-run-1234",
				Workflow: nebulav1.Workflow{
					Parameters: relayv1beta1.NewUnstructuredObject(map[string]interface{}{
						"Hello": "World!",
					}),
					Name: "my-workflow",
					Steps: []*nebulav1.WorkflowStep{
						{
							Name:  "my-test-step",
							Image: "alpine:latest",
							Spec: relayv1beta1.NewUnstructuredObject(map[string]interface{}{
								"secret": map[string]interface{}{
									"$type": "Secret",
									"name":  "foo",
								},
								"connection": map[string]interface{}{
									"$type": "Connection",
									"type":  "aws",
									"name":  "test",
								},
								"param": map[string]interface{}{
									"$type": "Parameter",
									"name":  "Hello",
								},
							}),
							Input: []string{
								"trap : TERM INT",
								"sleep 600 & wait",
							},
						},
					},
				},
			},
		}
		require.NoError(t, e2e.ControllerRuntimeClient.Create(ctx, wr))

		// Wait for step to start. Could use a ListWatcher but meh.
		require.NoError(t, retry.Retry(ctx, 500*time.Millisecond, func() *retry.RetryError {
			if err := e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{
				Namespace: wr.GetNamespace(),
				Name:      wr.GetName(),
			}, wr); err != nil {
				return retry.RetryPermanent(err)
			}

			if wr.Status.Steps["my-test-step"].Status == string(obj.WorkflowRunStatusInProgress) {
				return retry.RetryPermanent(nil)
			}

			return retry.RetryTransient(fmt.Errorf("waiting for step to start"))
		}))

		// Pull the pod and get its IP.
		pod := &corev1.Pod{}
		require.NoError(t, retry.Retry(ctx, 500*time.Millisecond, func() *retry.RetryError {
			pods := &corev1.PodList{}
			if err := e2e.ControllerRuntimeClient.List(ctx, pods, client.InNamespace(wr.GetNamespace()), client.MatchingLabels{
				// TODO: We shouldn't really hardcode this.
				"tekton.dev/task": (&model.Step{Run: model.Run{ID: wr.Spec.Name}, Name: "my-test-step"}).Hash().HexEncoding(),
			}); err != nil {
				return retry.RetryPermanent(err)
			}

			if len(pods.Items) == 0 {
				return retry.RetryTransient(fmt.Errorf("waiting for pod"))
			}

			pod = &pods.Items[0]
			if pod.Status.PodIP == "" {
				return retry.RetryTransient(fmt.Errorf("waiting for pod IP"))
			} else if pod.Status.Phase == corev1.PodPending {
				return retry.RetryTransient(fmt.Errorf("waiting for pod to start"))
			}

			return retry.RetryPermanent(nil)
		}))

		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/spec", cfg.MetadataAPIURL), nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", pod.Status.PodIP)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var result evaluate.JSONResultEnvelope
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		assert.True(t, result.Complete)
		assert.Equal(t, map[string]interface{}{
			"secret": "Hello",
			"connection": map[string]interface{}{
				"accessKeyID":     "AKIA123456789",
				"secretAccessKey": "very-nice-key",
			},
			"param": "World!",
		}, result.Value.Data)
	})
}
