package lfs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestService_Serve(t *testing.T) {
	content := "served content"
	oid := sha256Hex(content)
	size := len(content)
	const href = "https://lfs.example.com/repo/info/lfs/objects"
	readWrite := Access{Repo: "repo", Href: href, CanRead: true, CanWrite: true}

	serve := func(s *Service, access Access, method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, href+target, strings.NewReader(body))
		r.SetBasicAuth("user", "token")
		w := httptest.NewRecorder()
		s.Serve(discardLogger, w, r, access)
		return w
	}
	requireError := func(t *testing.T, w *httptest.ResponseRecorder, wantCode int) {
		t.Helper()
		require.Equal(t, wantCode, w.Code, w.Body.String())
		assert.Equal(t, MediaType, w.Header().Get("Content-Type"))
		var body struct{ Message string }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.NotEmpty(t, body.Message)
	}
	batch := func(operation string) string {
		return `{"operation":"` + operation + `","objects":[{"oid":"` + oid + `","size":` + strconv.Itoa(size) + `}]}`
	}
	verify := `{"oid":"` + oid + `","size":` + strconv.Itoa(size) + `}`

	t.Run("proxied transfer", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://")}

		w := serve(s, readWrite, http.MethodPost, "/batch", batch(OperationUpload))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, MediaType, w.Header().Get("Content-Type"))
		var resp BatchResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Len(t, resp.Objects, 1)
		authz := "Basic dXNlcjp0b2tlbg=="
		assert.True(t, resp.Objects[0].Authenticated)
		assert.Equal(t, Action{
			Href: href + "/" + oid,
			Header: map[string]string{
				"Authorization":         authz,
				"Content-Type":          "application/octet-stream",
				"x-amz-checksum-sha256": "97OiZ/pYWtQ/MOiPfTFzQY0nD/6zXoSoWwc00FhqN48=",
			},
		}, resp.Objects[0].Actions["upload"])
		assert.Equal(t, Action{Href: href + "/verify", Header: map[string]string{"Authorization": authz}}, resp.Objects[0].Actions["verify"])

		requireError(t, serve(s, readWrite, http.MethodPut, "/"+oid, strings.Repeat("x", size)), http.StatusUnprocessableEntity)
		requireError(t, serve(s, readWrite, http.MethodPost, "/verify", verify), http.StatusNotFound)

		w = serve(s, readWrite, http.MethodPut, "/"+oid, content)
		assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		w = serve(s, readWrite, http.MethodPost, "/verify", verify)
		assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

		w = serve(s, readWrite, http.MethodGet, "/"+oid, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
		assert.Equal(t, strconv.Itoa(size), w.Header().Get("Content-Length"))
		assert.Equal(t, content, w.Body.String())
	})

	t.Run("presigned transfer redirects", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newS3PresignBackend(t)}

		w := serve(s, readWrite, http.MethodPut, "/"+oid, "")
		require.Equal(t, http.StatusTemporaryRedirect, w.Code, w.Body.String())
		assert.Equal(t, "97OiZ/pYWtQ/MOiPfTFzQY0nD/6zXoSoWwc00FhqN48=", w.Header().Get("x-amz-checksum-sha256"))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, w.Header().Get("Location"), strings.NewReader(content))
		require.NoError(t, err)
		req.Header.Set("x-amz-checksum-sha256", w.Header().Get("x-amz-checksum-sha256"))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)

		w = serve(s, readWrite, http.MethodPost, "/verify", verify)
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

		w = serve(s, readWrite, http.MethodGet, "/"+oid, "")
		require.Equal(t, http.StatusTemporaryRedirect, w.Code, w.Body.String())
		resp, err = http.Get(w.Header().Get("Location"))
		require.NoError(t, err)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		assert.Equal(t, content, string(got))
	})

	t.Run("checks permissions", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://")}
		readOnly := Access{Repo: "repo", Href: href, CanRead: true}
		requireError(t, serve(s, readOnly, http.MethodPost, "/batch", batch(OperationUpload)), http.StatusForbidden)
		requireError(t, serve(s, readOnly, http.MethodPut, "/"+oid, content), http.StatusForbidden)
		requireError(t, serve(s, readOnly, http.MethodPost, "/verify", verify), http.StatusForbidden)
		assert.Equal(t, http.StatusOK, serve(s, readOnly, http.MethodPost, "/batch", batch(OperationDownload)).Code)

		writeOnly := Access{Repo: "repo", Href: href, CanWrite: true}
		requireError(t, serve(s, writeOnly, http.MethodPost, "/batch", batch(OperationDownload)), http.StatusForbidden)
		requireError(t, serve(s, writeOnly, http.MethodGet, "/"+oid, ""), http.StatusForbidden)

		_, err := s.Upload(t.Context(), discardLogger, "repo", oid, int64(size), strings.NewReader(content))
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, serve(s, readOnly, http.MethodGet, "/"+oid, "").Code)
	})

	t.Run("rejects invalid requests", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://")}
		requireError(t, serve(s, readWrite, http.MethodPost, "/batch", "{"), http.StatusBadRequest)
		requireError(t, serve(s, readWrite, http.MethodPost, "/batch", `{"operation":"delete"}`), http.StatusBadRequest)
		requireError(t, serve(s, readWrite, http.MethodPost, "/verify", "{"), http.StatusBadRequest)
		requireError(t, serve(s, readWrite, http.MethodGet, "/not-an-oid", ""), http.StatusBadRequest)
		requireError(t, serve(s, readWrite, http.MethodDelete, "/"+oid, ""), http.StatusMethodNotAllowed)
	})
}
