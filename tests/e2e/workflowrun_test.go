package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/puppetlabs/relay-core/pkg/admission"
	nebulav1 "github.com/puppetlabs/relay-core/pkg/apis/nebula.puppet.com/v1"
	relayv1beta1 "github.com/puppetlabs/relay-core/pkg/apis/relay.sh/v1beta1"
	"github.com/puppetlabs/relay-core/pkg/expr/evaluate"
	"github.com/puppetlabs/relay-core/pkg/model"
	"github.com/puppetlabs/relay-core/pkg/obj"
	"github.com/puppetlabs/relay-core/pkg/util/retry"
	"github.com/puppetlabs/relay-core/pkg/util/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func TestWorkflowRunWithTenantVolumeClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	WithConfig(t, ctx, []ConfigOption{
		ConfigWithMetadataAPI,
		ConfigWithTenantReconciler,
		ConfigWithWorkflowRunReconciler,
	}, func(cfg *Config) {
		hnd := testServerInjectorHandler{&webhook.Admission{Handler: admission.NewVolumeClaimHandler()}}
		cfg.Manager.SetFields(hnd)

		s := httptest.NewServer(hnd)
		defer s.Close()

		testutil.WithServiceBoundToHostHTTP(t, ctx, e2e.RESTConfig, e2e.Interface, s.URL, metav1.ObjectMeta{Namespace: cfg.Namespace.GetName()}, func(caPEM []byte, svc *corev1.Service) {
			// Set up webhook configuration in API server.
			handler := &admissionregistrationv1beta1.MutatingWebhookConfiguration{
				TypeMeta: metav1.TypeMeta{
					// Required for conversion during install, below.
					APIVersion: admissionregistrationv1beta1.SchemeGroupVersion.Identifier(),
					Kind:       "MutatingWebhookConfiguration",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "volume-claim",
				},
				Webhooks: []admissionregistrationv1beta1.MutatingWebhook{
					{
						Name: "volume-claim.admission.controller.relay.sh",
						ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
							Service: &admissionregistrationv1beta1.ServiceReference{
								Namespace: svc.GetNamespace(),
								Name:      svc.GetName(),
							},
							CABundle: caPEM,
						},
						Rules: []admissionregistrationv1beta1.RuleWithOperations{
							{
								Operations: []admissionregistrationv1beta1.OperationType{
									admissionregistrationv1beta1.Create,
									admissionregistrationv1beta1.Update,
								},
								Rule: admissionregistrationv1beta1.Rule{
									APIGroups:   []string{""},
									APIVersions: []string{"v1"},
									Resources:   []string{"pods"},
								},
							},
						},
						FailurePolicy: func(fp admissionregistrationv1beta1.FailurePolicyType) *admissionregistrationv1beta1.FailurePolicyType {
							return &fp
						}(admissionregistrationv1beta1.Fail),
						SideEffects: func(se admissionregistrationv1beta1.SideEffectClass) *admissionregistrationv1beta1.SideEffectClass {
							return &se
						}(admissionregistrationv1beta1.SideEffectClassNone),
						ReinvocationPolicy: func(rp admissionregistrationv1beta1.ReinvocationPolicyType) *admissionregistrationv1beta1.ReinvocationPolicyType {
							return &rp
						}(admissionregistrationv1beta1.IfNeededReinvocationPolicy),
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"testing.relay.sh/tools-volume-claim": "true",
							},
						},
					},
				},
			}

			// Patch instead of Create because this object is cluster-scoped
			// so we want to overwrite previous test attempts.
			require.NoError(t, e2e.ControllerRuntimeClient.Patch(ctx, handler, client.Apply, client.ForceOwnership, client.FieldOwner("relay-e2e")))
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				assert.NoError(t, e2e.ControllerRuntimeClient.Delete(ctx, handler))
			}()

			size, _ := resource.ParseQuantity("50Mi")
			storageClassName := "relay-hostpath"
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
			require.Equal(t, tenant.Spec.NamespaceTemplate.Metadata.Name, tenant.Status.Namespace)
			require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.Status.Namespace}, &ns))

			var job batchv1.Job
			require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + model.ToolInjectionVolumeClaimSuffixReadOnlyMany, Namespace: tenant.Status.Namespace}, &job))

			var pvcw corev1.PersistentVolumeClaim
			require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + model.ToolInjectionVolumeClaimSuffixReadWriteOnce, Namespace: tenant.Status.Namespace}, &pvcw))

			var pvcr corev1.PersistentVolumeClaim
			require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + model.ToolInjectionVolumeClaimSuffixReadOnlyMany, Namespace: tenant.Status.Namespace}, &pvcr))

			var pv corev1.PersistentVolume
			require.NoError(t, e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: tenant.GetName() + model.ToolInjectionVolumeClaimSuffixReadOnlyMany}, &pv))

			// Set a secret and connection for this workflow to look up.
			cfg.Vault.SetSecret(t, tenant.GetName(), "foo", "Hello")
			cfg.Vault.SetConnection(t, "my-domain-id", "aws", "test", map[string]string{
				"accessKeyID":     "AKIA123456789",
				"secretAccessKey": "very-nice-key",
			})

			wr := &nebulav1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: tenant.Status.Namespace,
					Name:      "my-test-run",
					Annotations: map[string]string{
						model.RelayVaultEngineMountAnnotation:    cfg.Vault.SecretsPath,
						model.RelayVaultConnectionPathAnnotation: "connections/my-domain-id",
						model.RelayVaultSecretPathAnnotation:     "workflows/" + tenant.GetName(),
						model.RelayDomainIDAnnotation:            "my-domain-id",
						model.RelayTenantIDAnnotation:            tenant.GetName(),
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
								Image: "gcr.io/nebula-tasks/entrypoint:test",
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
	})
}

// TestWorkflowRun tests that an instance of the controller, when given a run to
// process, correctly sets up a Tekton pipeline and that the resulting pipeline
// should be able to access a metadata API service.
func TestWorkflowRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	WithConfig(t, ctx, []ConfigOption{
		ConfigWithMetadataAPI,
		ConfigWithWorkflowRunReconciler,
	}, func(cfg *Config) {
		// Set a secret and connection for this workflow to look up.
		cfg.Vault.SetSecret(t, "my-tenant-id", "foo", "Hello")
		cfg.Vault.SetSecret(t, "my-tenant-id", "accessKeyId", "AKIA123456789")
		cfg.Vault.SetSecret(t, "my-tenant-id", "secretAccessKey", "that's-a-very-nice-key-you-have-there")
		cfg.Vault.SetConnection(t, "my-domain-id", "aws", "test", map[string]string{
			"accessKeyID":     "AKIA123456789",
			"secretAccessKey": "that's-a-very-nice-key-you-have-there",
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
							Env: relayv1beta1.NewUnstructuredObject(map[string]interface{}{
								"AWS_ACCESS_KEY_ID": map[string]interface{}{
									"$type": "Secret",
									"name":  "accessKeyId",
								},
								"AWS_SECRET_ACCESS_KEY": map[string]interface{}{
									"$type": "Secret",
									"name":  "secretAccessKey",
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
				"secretAccessKey": "that's-a-very-nice-key-you-have-there",
			},
			"param": "World!",
		}, result.Value.Data)

		req, err = http.NewRequest(http.MethodGet, fmt.Sprintf("%s/environment", cfg.MetadataAPIURL), nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", pod.Status.PodIP)

		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		assert.True(t, result.Complete)
		assert.Equal(t, map[string]interface{}{
			"AWS_ACCESS_KEY_ID":     "AKIA123456789",
			"AWS_SECRET_ACCESS_KEY": "that's-a-very-nice-key-you-have-there",
		}, result.Value.Data)

		req, err = http.NewRequest(http.MethodGet, fmt.Sprintf("%s/environment/AWS_ACCESS_KEY_ID", cfg.MetadataAPIURL), nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", pod.Status.PodIP)

		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		assert.True(t, result.Complete)
		assert.Equal(t, "AKIA123456789", result.Value.Data)

		req, err = http.NewRequest(http.MethodGet, fmt.Sprintf("%s/environment/AWS_SECRET_ACCESS_KEY", cfg.MetadataAPIURL), nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", pod.Status.PodIP)

		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		assert.True(t, result.Complete)
		assert.Equal(t, "that's-a-very-nice-key-you-have-there", result.Value.Data)
	})
}

func TestWorkflowRunWithoutSteps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	WithConfig(t, ctx, []ConfigOption{
		ConfigWithWorkflowRunReconciler,
	}, func(cfg *Config) {
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
					Name:  "my-workflow",
					Steps: []*nebulav1.WorkflowStep{},
				},
			},
		}
		require.NoError(t, e2e.ControllerRuntimeClient.Create(ctx, wr))

		require.NoError(t, retry.Retry(ctx, 500*time.Millisecond, func() *retry.RetryError {
			if err := e2e.ControllerRuntimeClient.Get(ctx, client.ObjectKey{Name: wr.GetName(), Namespace: wr.GetNamespace()}, wr); err != nil {
				if k8serrors.IsNotFound(err) {
					retry.RetryTransient(fmt.Errorf("waiting for initial workflow run"))
				}

				return retry.RetryPermanent(err)
			}

			if wr.Status.Status == "" {
				return retry.RetryTransient(fmt.Errorf("waiting for workflow run status"))
			}

			return retry.RetryPermanent(nil)
		}))

		require.Equal(t, string(obj.WorkflowRunStatusSuccess), wr.Status.Status)
		require.NotNil(t, wr.Status.StartTime)
		require.NotNil(t, wr.Status.CompletionTime)
	})
}
