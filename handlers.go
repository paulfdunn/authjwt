// Package authjwt implements JWT authentication.

package authjwt

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/paulfdunn/go-helper/logh"
	"github.com/paulfdunn/go-helper/neth/httph"
)

// AuditWriter is used to wrap the http.ResponseWriter passed to handlers in order to
// store information that is then written to the audit log as the handler exits.
// Applications using this package need to populate the Message as is done in these handlers
// in order for messages to show up in the audit log. Best practice is to only add logging
// information to the audit log once all validations are complete and the command is returning
// good status. Other information should be logged to an application log.
type AuditWriter struct {
	http.ResponseWriter
	Message    string
	StatusCode int
}

func (aw *AuditWriter) WriteHeader(status int) {
	aw.StatusCode = status
	aw.ResponseWriter.WriteHeader(status)
}

// HandlerFuncNoAuthWrapper is a basic wrapper that DOES NOT authenticate, but does
// handle audit logging (logging for all DELETE/POST/PUT methods)
func HandlerFuncNoAuthWrapper(hf func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		aw := &AuditWriter{w, "", 0}
		hf(aw, r)
		if r.Method == http.MethodDelete || r.Method == http.MethodPost || r.Method == http.MethodPut {
			logh.Map[config.AuditLogName].Printf(logh.Audit, "status: %d| req:%+v| msg: %s|\n\n", aw.StatusCode, r, aw.Message)
		}
	}
}

// HandlerFuncAuthJWTWrapper is a basic wrapper that verifies the call is authenticated.
// Use this directly, or for additional verification of Authorizations, Role, etc., use this as an example.
// Note this wrapper also handles audit logging (logging for all DELETE/POST/PUT methods)
func HandlerFuncAuthJWTWrapper(hf func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		aw := &AuditWriter{w, "", 0}
		var err error
		if config.DataSourcePath != "" {
			_, err = Authenticated(aw, r)
		} else {
			_, err = AuthenticatedNoTokenInvalidation(aw, r)
		}
		if err != nil {
			return
		}
		hf(aw, r)
		if r.Method == http.MethodDelete || r.Method == http.MethodPost || r.Method == http.MethodPut {
			logh.Map[config.AuditLogName].Printf(logh.Audit, "status: %d| req:%+v| msg: %s|\n\n", aw.StatusCode, r, aw.Message)
		}
	}
}

// handlerCreateOrUpdate is the handler to create/update an auth (entry in kvsAuth). The handler
// will error if there is already an auth for the specified Email for create (http.MethodPost).
// Update (http.MethodPut) requires the user is logged in and provides a valid token.
func handlerCreateOrUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	em := ""
	pw := ""
	cred := Credential{Email: &em, Password: &pw}
	if err := httph.BodyUnmarshal(w, r, &cred); err != nil {
		lpf(logh.Error, "create error:%v", err)
		// WriteHeader provided by BodyUnmarshal
		return
	}

	// Either create or update require valid credentials in the body.
	auth, err := authGet(em)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// On create, the auth must not exist. On update, the user must be logged in.
	if r.Method == http.MethodPost {
		if auth.PasswordHash != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
	} else { // http.MethodPut
		_, err := Authenticated(w, r)
		if err != nil {
			return
		}
	}

	if err := cred.AuthCreate(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if aw, ok := w.(*AuditWriter); ok {
		aw.Message = fmt.Sprintf("credential create or update for email: %s", *cred.Email)
	}

	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusCreated)
	} else { // http.MethodPut
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlerDelete deletes the entries in kvsAuth and kvsToken for
// the specified Email.
func handlerDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// re-authenticate to get claims, in order to delete the auth and the token.
	claims, err := Authenticated(w, r)
	if err != nil {
		return
	}

	if aw, ok := w.(*AuditWriter); ok {
		aw.Message = fmt.Sprintf("auth and tokens deleted for email: %s", claims.Email)
	}

	// Remove all users tokens then delete the kvsAuth
	// handlerLogoutCommon sets http.StatusNoContent
	handlerLogoutCommon(w, r, true)
	if _, err := kvsAuth.Delete(claims.Email); err != nil {
		lpf(logh.Error, "kvsAuth.Delete error: %+v", err)
	}
}

// handlerInfo will return an Info object for the caller.
func handlerInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// re-authenticate to get claims, in order to get claims.
	claims, err := Authenticated(w, r)
	if err != nil {
		return
	}
	c, err := userTokens(claims.Email, false)
	if err != nil {
		lpf(logh.Error, "userTokens error:%v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	info := Info{OutstandingTokens: c}
	b, err := json.Marshal(info)
	if err != nil {
		lpf(logh.Error, "json.Marshal error:%v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil {
		lpf(logh.Error, "w.Write error:%+v", err)
	}
}

// handlerLogin will validate a callers credentials and, if the credentials are
// valid, will return a JWT token for the caller.
func handlerLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	} else if r.Body == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	em := ""
	pw := ""
	cred := Credential{Email: &em, Password: &pw}
	if err := httph.BodyUnmarshal(w, r, &cred); err != nil {
		lpf(logh.Error, "login error:%v", err)
		// WriteHeader provided by BodyUnmarshal
		return
	}

	auth, err := authGet(*cred.Email)
	if err != nil {
		lpf(logh.Error, "authGet error:%v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := passwordVerifyHash(*cred.Password, auth.PasswordHash); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	tokenString, err := authTokenStringCreate(*cred.Email)
	if err != nil {
		lpf(logh.Error, "authTokenStringCreate error:%v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if aw, ok := w.(*AuditWriter); ok {
		aw.Message = fmt.Sprintf("login for email: %s", *cred.Email)
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(tokenString)); err != nil {
		lpf(logh.Error, "w.Write error:%+v", err)
	}
}

// handlerLogout will delete the token the caller is currently using,
// effectively logging them out as the token is no longer valid.
func handlerLogout(w http.ResponseWriter, r *http.Request) {
	handlerLogoutCommon(w, r, false)
}

// handlerLogoutAll will delete all tokens for the current caller,
// effectively logging them out of all sessions, as none of their issued
// tokens will be valid.
func handlerLogoutAll(w http.ResponseWriter, r *http.Request) {
	handlerLogoutCommon(w, r, true)
}

func handlerLogoutCommon(w http.ResponseWriter, r *http.Request, logoutAll bool) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// re-authenticate to get claims, in order to delete the token.
	claims, err := Authenticated(w, r)
	if err != nil {
		return
	}

	if logoutAll {
		_, err := userTokens(claims.Email, true)
		if err != nil {
			lpf(logh.Error, "userTokens error:%v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if aw, ok := w.(*AuditWriter); ok {
			aw.Message = fmt.Sprintf("all tokens deleted for email: %s", claims.Email)
		}
	} else {
		n, err := kvsToken.Delete(claims.tokenKVSKey())
		if err != nil {
			lpf(logh.Error, "kvsToken.Delete error:%v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if aw, ok := w.(*AuditWriter); ok {
			aw.Message = fmt.Sprintf("%d tokens deleted for email: %s", n, claims.Email)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlerRefresh deletes the callers current token and returns
// a new token.
func handlerRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	} else if r.Body == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// re-authenticate to get claims.
	claims, err := Authenticated(w, r)
	if err != nil {
		return
	}

	tokenString, err := authTokenStringCreate(claims.Email)
	if err != nil {
		lpf(logh.Error, "authTokenStringCreate error:%v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	n, err := kvsToken.Delete(claims.tokenKVSKey())
	if err != nil {
		lpf(logh.Error, "kvsToken.Delete error:%v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if aw, ok := w.(*AuditWriter); ok {
		aw.Message = fmt.Sprintf("%d tokens deleted during token refresh for email: %s", n, claims.Email)
	}
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(tokenString)); err != nil {
		lpf(logh.Error, "w.Write error:%+v", err)
	}
}
