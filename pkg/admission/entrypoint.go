package admission

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// TODO Where does this belong?
const (
	VolumeClaimAnnotation = "controller.relay.sh/volume-claim"
)

type EntrypointHandler struct {
	decoder *admission.Decoder
}

var _ admission.Handler = &EntrypointHandler{}
var _ admission.DecoderInjector = &EntrypointHandler{}

func (eh *EntrypointHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := eh.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if claim, ok := pod.ObjectMeta.GetAnnotations()[VolumeClaimAnnotation]; ok {
		if pod.Spec.Volumes == nil {
			pod.Spec.Volumes = make([]corev1.Volume, 0)
		}

		for _, volume := range pod.Spec.Volumes {
			if volume.Name == "entrypoint" {
				return admission.Allowed("Already modified")
			}
		}

		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "entrypoint",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim,
					ReadOnly:  true,
				},
			},
		})

		b, err := json.Marshal(pod)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		return admission.PatchResponseFromRaw(req.Object.Raw, b)
	}

	return admission.Allowed("Unnecessary")
}

func (eh *EntrypointHandler) InjectDecoder(d *admission.Decoder) error {
	eh.decoder = d
	return nil
}

type EntrypointHandlerOption func(eh *EntrypointHandler)

func NewEntrypointHandler(opts ...EntrypointHandlerOption) *EntrypointHandler {
	eh := &EntrypointHandler{}

	for _, opt := range opts {
		opt(eh)
	}

	return eh
}
