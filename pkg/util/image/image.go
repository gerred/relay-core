package image

import (
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/puppetlabs/relay-core/pkg/model"
)

func ImageData(ref name.Reference, img v1.Image) ([]string, []string, name.Digest, error) {
	digest, err := img.Digest()
	if err != nil {
		return nil, nil, name.Digest{}, err
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, nil, name.Digest{}, err
	}

	d, err := name.NewDigest(ref.Context().String()+"@"+digest.String(), name.WeakValidation)
	if err != nil {
		return nil, nil, name.Digest{}, err
	}

	return cfg.Config.Entrypoint, cfg.Config.Cmd, d, nil
}

func ImageEntrypoint(image string, command []string, args []string) ([]string, error) {
	ref, err := name.ParseReference(image, name.WeakValidation)
	if err != nil {
		return nil, err
	}

	img, err := remote.Image(ref)
	if err != nil {
		return nil, err
	}

	var argsForEntrypoint []string

	if len(command) > 0 && len(command[0]) > 0 {
		argsForEntrypoint = append(argsForEntrypoint, model.EntrypointCommandFlag, command[0], model.EntrypointCommandArgSeparator)
		argsForEntrypoint = append(argsForEntrypoint, command[1:]...)
		argsForEntrypoint = append(argsForEntrypoint, args...)
	} else {
		ep, cmd, _, err := ImageData(ref, img)
		if err != nil {
			return nil, err
		}

		if len(ep) > 0 {
			argsForEntrypoint = append(argsForEntrypoint, model.EntrypointCommandFlag, ep[0], model.EntrypointCommandArgSeparator)
			argsForEntrypoint = append(argsForEntrypoint, ep[1:]...)
			argsForEntrypoint = append(argsForEntrypoint, cmd[0:]...)
			argsForEntrypoint = append(argsForEntrypoint, args...)
		} else {
			argsForEntrypoint = append(argsForEntrypoint, model.EntrypointCommandFlag, cmd[0], model.EntrypointCommandArgSeparator)
			argsForEntrypoint = append(argsForEntrypoint, cmd[1:]...)
			argsForEntrypoint = append(argsForEntrypoint, args...)
		}
	}

	return argsForEntrypoint, nil
}
