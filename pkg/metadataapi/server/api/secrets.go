package api

import (
	"net/http"

	"github.com/puppetlabs/horsehead/v2/encoding/transfer"
	utilapi "github.com/puppetlabs/horsehead/v2/httputil/api"
	"github.com/puppetlabs/nebula-sdk/pkg/secrets"
	"github.com/puppetlabs/nebula-tasks/pkg/metadataapi/errors"
	"github.com/puppetlabs/nebula-tasks/pkg/metadataapi/server/middleware"
)

func (s *Server) GetSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	managers := middleware.Managers(r)
	sm := managers.Secrets()

	name, _ := middleware.Var(r, "name")

	sec, err := sm.Get(ctx, name)
	if err != nil {
		utilapi.WriteError(ctx, w, ModelReadError(err))
		return
	}

	ev, verr := transfer.EncodeJSON([]byte(sec.Value))
	if verr != nil {
		utilapi.WriteError(ctx, w, errors.NewModelReadError().WithCause(verr).Bug())
		return
	}

	env := &secrets.Secret{
		Key:   sec.Name,
		Value: ev,
	}

	utilapi.WriteObjectOK(ctx, w, env)
}
