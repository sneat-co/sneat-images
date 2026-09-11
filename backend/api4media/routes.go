// Copyright 2026 Sneat.app

package api4media

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/sneat-co/sneat-go-core/apicore"
	"github.com/sneat-co/sneat-go-core/apicore/verify"
	"github.com/sneat-co/sneat-go-core/extension"
	"github.com/sneat-co/sneat-go-core/facade"
	"github.com/sneat-co/sneat-images/backend/facade4media"
)

type Handler struct {
	Service      *facade4media.Service
	OriginSecret string
}

// ServeHTTP lets executable hosts construct the infrastructure lazily while
// keeping endpoint dispatch in the owning media module.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case http.MethodPost + " /v0/media/uploads":
		h.beginUpload(w, r)
	case http.MethodPost + " /v0/media/uploads/finalize":
		h.finalizeUpload(w, r)
	case http.MethodPost + " /v0/media/links":
		h.link(w, r)
	case http.MethodPost + " /v0/media/links/remove":
		h.unlink(w, r)
	case http.MethodPost + " /v0/media/links/restore":
		h.restore(w, r)
	case http.MethodPost + " /v0/media/access":
		h.access(w, r)
	case http.MethodGet + " /v0/media/origin", http.MethodHead + " /v0/media/origin":
		h.origin(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h Handler) RegisterRoutes(handle extension.HTTPHandleFunc) {
	handle(http.MethodPost, "/v0/media/uploads", h.beginUpload)
	handle(http.MethodPost, "/v0/media/uploads/finalize", h.finalizeUpload)
	handle(http.MethodPost, "/v0/media/links", h.link)
	handle(http.MethodPost, "/v0/media/links/remove", h.unlink)
	handle(http.MethodPost, "/v0/media/links/restore", h.restore)
	handle(http.MethodPost, "/v0/media/access", h.access)
	handle(http.MethodGet, "/v0/media/origin", h.origin)
	handle(http.MethodHead, "/v0/media/origin", h.origin)
}

func authenticatedRequest(w http.ResponseWriter, r *http.Request, request facade.Request) (facade.ContextWithUser, bool) {
	ctx, err := apicore.VerifyAuthenticatedRequestAndDecodeBody(w, r, verify.DefaultJsonWithAuthRequired, request)
	if err != nil {
		return nil, false
	}
	return ctx, true
}

func (h Handler) beginUpload(w http.ResponseWriter, r *http.Request) {
	var request facade4media.BeginUploadRequest
	ctx, ok := authenticatedRequest(w, r, &request)
	if !ok {
		return
	}
	response, err := h.Service.BeginUpload(ctx, ctx.User().GetUserID(), request)
	apicore.ReturnJSON(ctx, w, r, http.StatusCreated, err, &response)
}

func (h Handler) finalizeUpload(w http.ResponseWriter, r *http.Request) {
	var request facade4media.FinalizeUploadRequest
	ctx, ok := authenticatedRequest(w, r, &request)
	if !ok {
		return
	}
	response, err := h.Service.FinalizeUpload(ctx, ctx.User().GetUserID(), request)
	apicore.ReturnJSON(ctx, w, r, http.StatusOK, err, response)
}

func (h Handler) link(w http.ResponseWriter, r *http.Request) {
	var request facade4media.LinkRequest
	ctx, ok := authenticatedRequest(w, r, &request)
	if !ok {
		return
	}
	response, err := h.Service.Link(ctx, ctx.User().GetUserID(), request)
	apicore.ReturnJSON(ctx, w, r, http.StatusOK, err, &response)
}

func (h Handler) unlink(w http.ResponseWriter, r *http.Request) {
	var request facade4media.TargetRequest
	ctx, ok := authenticatedRequest(w, r, &request)
	if !ok {
		return
	}
	response, err := h.Service.Unlink(ctx, ctx.User().GetUserID(), request.Target, request.Role)
	apicore.ReturnJSON(ctx, w, r, http.StatusOK, err, &response)
}

func (h Handler) restore(w http.ResponseWriter, r *http.Request) {
	var request facade4media.RestoreRequest
	ctx, ok := authenticatedRequest(w, r, &request)
	if !ok {
		return
	}
	response, err := h.Service.Restore(ctx, ctx.User().GetUserID(), request.MediaID, request.Target, request.Role)
	apicore.ReturnJSON(ctx, w, r, http.StatusOK, err, &response)
}

type accessRequest struct {
	MediaID string              `json:"mediaID"`
	Role    string              `json:"role"`
	Target  models4media.Target `json:"target"`
}

func (v accessRequest) Validate() error {
	if v.MediaID == "" || v.Role == "" {
		return fmt.Errorf("mediaID and role are required")
	}
	return v.Target.Validate()
}

type accessResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (h Handler) access(w http.ResponseWriter, r *http.Request) {
	var request accessRequest
	ctx, ok := authenticatedRequest(w, r, &request)
	if !ok {
		return
	}
	if err := h.Service.AuthorizeMediaAccess(ctx, ctx.User().GetUserID(), request.MediaID, request.Target, request.Role); err != nil {
		apicore.ReturnJSON(ctx, w, r, http.StatusForbidden, err, nil)
		return
	}
	token, expires, err := h.Service.AccessToken(ctx.User().GetUserID(), request.MediaID, request.Target, 15*time.Minute)
	apicore.ReturnJSON(ctx, w, r, http.StatusOK, err, &accessResponse{Token: token, ExpiresAt: expires})
}

func (h Handler) origin(w http.ResponseWriter, r *http.Request) {
	provided := []byte(r.Header.Get("Authorization"))
	expected := []byte("Bearer " + h.OriginSecret)
	if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	mediaID := r.URL.Query().Get("mediaID")
	if mediaID == "" {
		http.Error(w, "mediaID is required", http.StatusBadRequest)
		return
	}
	asset, err := h.Service.GetAsset(r.Context(), mediaID)
	if err != nil {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}
	if asset.Status != models4media.AssetStatusReady && asset.Status != models4media.AssetStatusOrphaned {
		http.Error(w, "media not ready", http.StatusConflict)
		return
	}
	if !originAccessAllowed(asset.Access, r.Header.Get("X-Media-Access-Verified")) {
		log.Printf("refuse media origin access mediaID=%s access=%q", mediaID, asset.Access)
		http.Error(w, "media access unavailable", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	capability, err := h.Service.Blob.BeginRead(r.Context(), asset.Storage.ObjectKey, asset.Storage.Generation, time.Now().UTC().Add(2*time.Minute))
	if err != nil {
		log.Printf("create media original read capability mediaID=%s objectKey=%s err=%v", mediaID, asset.Storage.ObjectKey, err)
		http.Error(w, "origin unavailable", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, capability.URL, http.StatusTemporaryRedirect)
}

func originAccessAllowed(access models4media.Access, verified string) bool {
	switch access {
	case models4media.AccessPublic:
		return true
	case models4media.AccessPrivate:
		return verified == "private"
	default:
		return false
	}
}
