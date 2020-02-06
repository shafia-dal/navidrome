package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deluan/navidrome/consts"
	"github.com/deluan/navidrome/engine/auth"
	"github.com/deluan/navidrome/log"
	"github.com/deluan/navidrome/model"
	"github.com/deluan/rest"
	"github.com/dgrijalva/jwt-go"
	"github.com/go-chi/jwtauth"
	"github.com/google/uuid"
)

var (
	once         sync.Once
	ErrFirstTime = errors.New("no users created")
)

func Login(ds model.DataStore) func(w http.ResponseWriter, r *http.Request) {
	auth.InitTokenAuth(ds)

	return func(w http.ResponseWriter, r *http.Request) {
		username, password, err := getCredentialsFromBody(r)
		if err != nil {
			log.Error(r, "Parsing request body", err)
			rest.RespondWithError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		handleLogin(ds, username, password, w, r)
	}
}

func handleLogin(ds model.DataStore, username string, password string, w http.ResponseWriter, r *http.Request) {
	user, err := validateLogin(ds.User(r.Context()), username, password)
	if err != nil {
		rest.RespondWithError(w, http.StatusInternalServerError, "Unknown error authentication user. Please try again")
		return
	}
	if user == nil {
		log.Warn(r, "Unsuccessful login", "username", username, "request", r.Header)
		rest.RespondWithError(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}

	tokenString, err := auth.CreateToken(user)
	if err != nil {
		rest.RespondWithError(w, http.StatusInternalServerError, "Unknown error authenticating user. Please try again")
		return
	}
	rest.RespondWithJSON(w, http.StatusOK,
		map[string]interface{}{
			"message":  "User '" + username + "' authenticated successfully",
			"token":    tokenString,
			"name":     user.Name,
			"username": username,
			"isAdmin":  user.IsAdmin,
			"version":  consts.Version(),
		})
}

func getCredentialsFromBody(r *http.Request) (username string, password string, err error) {
	data := make(map[string]string)
	decoder := json.NewDecoder(r.Body)
	if err = decoder.Decode(&data); err != nil {
		log.Error(r, "parsing request body", err)
		err = errors.New("Invalid request payload")
		return
	}
	username = data["username"]
	password = data["password"]
	return username, password, nil
}

func CreateAdmin(ds model.DataStore) func(w http.ResponseWriter, r *http.Request) {
	auth.InitTokenAuth(ds)

	return func(w http.ResponseWriter, r *http.Request) {
		username, password, err := getCredentialsFromBody(r)
		if err != nil {
			log.Error(r, "parsing request body", err)
			rest.RespondWithError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		c, err := ds.User(r.Context()).CountAll()
		if err != nil {
			rest.RespondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if c > 0 {
			rest.RespondWithError(w, http.StatusForbidden, "Cannot create another first admin")
			return
		}
		err = createDefaultUser(r.Context(), ds, username, password)
		if err != nil {
			rest.RespondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		handleLogin(ds, username, password, w, r)
	}
}

func createDefaultUser(ctx context.Context, ds model.DataStore, username, password string) error {
	id, _ := uuid.NewRandom()
	log.Warn("Creating initial user", "user", username)
	now := time.Now()
	initialUser := model.User{
		ID:          id.String(),
		UserName:    username,
		Name:        strings.Title(username),
		Email:       "",
		Password:    password,
		IsAdmin:     true,
		LastLoginAt: &now,
	}
	err := ds.User(ctx).Put(&initialUser)
	if err != nil {
		log.Error("Could not create initial user", "user", initialUser, err)
	}
	return nil
}

func validateLogin(userRepo model.UserRepository, userName, password string) (*model.User, error) {
	u, err := userRepo.FindByUsername(userName)
	if err == model.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.Password != password {
		return nil, nil
	}
	err = userRepo.UpdateLastLoginAt(u.ID)
	if err != nil {
		log.Error("Could not update LastLoginAt", "user", userName)
	}
	return u, nil
}

func contextWithUser(ctx context.Context, ds model.DataStore, claims jwt.MapClaims) context.Context {
	userName := claims["sub"].(string)
	user, _ := ds.User(ctx).FindByUsername(userName)
	return context.WithValue(ctx, "user", user)
}

func getToken(ds model.DataStore, ctx context.Context) (*jwt.Token, error) {
	token, claims, err := jwtauth.FromContext(ctx)

	valid := err == nil && token != nil && token.Valid
	valid = valid && claims["sub"] != nil
	if valid {
		return token, nil
	}

	c, err := ds.User(ctx).CountAll()
	firstTime := c == 0 && err == nil
	if firstTime {
		return nil, ErrFirstTime
	}
	return nil, errors.New("invalid authentication")
}

func Authenticator(ds model.DataStore) func(next http.Handler) http.Handler {
	auth.InitTokenAuth(ds)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := getToken(ds, r.Context())
			if err == ErrFirstTime {
				rest.RespondWithJSON(w, http.StatusUnauthorized, map[string]string{"message": ErrFirstTime.Error()})
				return
			}
			if err != nil {
				rest.RespondWithError(w, http.StatusUnauthorized, "Not authenticated")
				return
			}

			claims := token.Claims.(jwt.MapClaims)

			newCtx := contextWithUser(r.Context(), ds, claims)
			newTokenString, err := auth.TouchToken(token)
			if err != nil {
				log.Error(r, "signing new token", err)
				rest.RespondWithError(w, http.StatusUnauthorized, "Not authenticated")
				return
			}

			w.Header().Set("Authorization", newTokenString)
			next.ServeHTTP(w, r.WithContext(newCtx))
		})
	}
}
