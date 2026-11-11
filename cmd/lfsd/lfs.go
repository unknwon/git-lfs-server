package main

import (
	"fmt"
	"net/http"

	"github.com/flamego/flamego"

	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/lfs"
)

// serveLFS hands the request to the *lfs.Service of the forge host, with the
// permission resolved by the authorize middleware.
func serveLFS(externalURL string, services map[string]*lfs.Service) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, perm forge.Permission) {
		service, ok := services[hostFromContext(c)]
		if !ok {
			logger.ErrorContext(c.Request().Context(), "No storage backend configured for host")
			lfs.WriteError(c.ResponseWriter(), http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
			return
		}

		repoName := repoNameFromContext(c)
		service.Serve(logger.Scoped("lfs").Logger, c.ResponseWriter(), c.Request().Request, lfs.Access{
			Repo:     repoName,
			Href:     fmt.Sprintf("%s/%s/info/lfs/objects", externalURL, repoName),
			CanRead:  perm == forge.PermissionRead || perm == forge.PermissionWrite,
			CanWrite: perm == forge.PermissionWrite,
		})
	}
}
