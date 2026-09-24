package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

const (
	defaultSessionIDKey      = "SESSIONID"
	defaultSessionExpiresKey = "EXPIRES"
	defaultUserIDKey         = "USERID"
	defaultUsernameKey       = "USERNAME"
	bcryptDefaultCost        = bcrypt.MinCost
)

var fallbackImage = "../img/NoImage.jpg"

type UserModel struct {
	ID             int64  `db:"id"`
	Name           string `db:"name"`
	DisplayName    string `db:"display_name"`
	Description    string `db:"description"`
	HashedPassword string `db:"password"`
}

type User struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Theme       Theme  `json:"theme,omitempty"`
	IconHash    string `json:"icon_hash,omitempty"`
}

type Theme struct {
	ID       int64 `json:"id"`
	DarkMode bool  `json:"dark_mode"`
}

type ThemeModel struct {
	ID       int64 `db:"id"`
	UserID   int64 `db:"user_id"`
	DarkMode bool  `db:"dark_mode"`
}

type PostUserRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Password is non-hashed password.
	Password string               `json:"password"`
	Theme    PostUserRequestTheme `json:"theme"`
}

type PostUserRequestTheme struct {
	DarkMode bool `json:"dark_mode"`
}

type LoginRequest struct {
	Username string `json:"username"`
	// Password is non-hashed password.
	Password string `json:"password"`
}

type PostIconRequest struct {
	Image []byte `json:"image"`
}

type PostIconResponse struct {
	ID int64 `json:"id"`
}

func getIconHandler(c echo.Context) error {
	ctx := c.Request().Context()

	username := c.Param("username")

	// No transaction: every read here is a single statement, and on cache hits there is no DB access at all.
	user, err := getUserModelByName(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	// Conditional GET (spec: the server MAY answer 304 when If-None-Match equals the quoted sha256
	// of the current icon; it MUST NOT answer 304 for a non-conditional request).
	if inm := c.Request().Header.Get("If-None-Match"); inm != "" {
		iconHash, err := getIconHash(ctx, dbConn, user.ID)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user icon: "+err.Error())
		}
		if inm == `"`+iconHash+`"` {
			return c.NoContent(http.StatusNotModified)
		}
	}

	image, err := getIconImage(ctx, user.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user icon: "+err.Error())
	}
	return c.Blob(http.StatusOK, "image/jpeg", image)
}

func postIconHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var req *PostIconRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM icons WHERE user_id = ?", userID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to delete old user icon: "+err.Error())
	}

	rs, err := tx.ExecContext(ctx, "INSERT INTO icons (user_id, image) VALUES (?, ?)", userID, req.Image)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert new user icon: "+err.Error())
	}

	iconID, err := rs.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted icon id: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}
	iconHashCache.Store(userID, computeIconHash(req.Image))
	iconImageCache.Store(userID, req.Image)

	return c.JSON(http.StatusCreated, &PostIconResponse{
		ID: iconID,
	})
}

func getMeHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	// Read-only: no transaction; users are insert-only and cached.
	userModel, err := getUserModelByID(ctx, dbConn, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "not found user that has the userid in session")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	user, err := fillUserResponse(ctx, dbConn, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	return c.JSON(http.StatusOK, user)
}

// ユーザ登録API
// POST /api/register
func registerHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	req := PostUserRequest{}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	if req.Name == "pipe" {
		return echo.NewHTTPError(http.StatusBadRequest, "the username 'pipe' is reserved")
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptDefaultCost)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to generate hashed password: "+err.Error())
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{
		Name:           req.Name,
		DisplayName:    req.DisplayName,
		Description:    req.Description,
		HashedPassword: string(hashedPassword),
	}

	result, err := tx.NamedExecContext(ctx, "INSERT INTO users (name, display_name, description, password) VALUES(:name, :display_name, :description, :password)", userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert user: "+err.Error())
	}

	userID, err := result.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted user id: "+err.Error())
	}

	userModel.ID = userID

	themeModel := ThemeModel{
		UserID:   userID,
		DarkMode: req.Theme.DarkMode,
	}
	if _, err := tx.NamedExecContext(ctx, "INSERT INTO themes (user_id, dark_mode) VALUES(:user_id, :dark_mode)", themeModel); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert user theme: "+err.Error())
	}

	if out, err := exec.Command("pdnsutil", "add-record", "u.isucon.local", req.Name, "A", "0", powerDNSSubdomainAddress).CombinedOutput(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, string(out)+": "+err.Error())
	}

	user, err := fillUserResponse(ctx, tx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusCreated, user)
}

// ユーザログインAPI
// POST /api/login
func loginHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	req := LoginRequest{}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	// usernameはUNIQUEなので一意に特定できる。usersはinsert-onlyなのでキャッシュ経由で読む(トランザクション不要)
	userModel, err := getUserModelByName(ctx, req.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid username or password")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	err = bcrypt.CompareHashAndPassword([]byte(userModel.HashedPassword), []byte(req.Password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid username or password")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to compare hash and password: "+err.Error())
	}

	sessionEndAt := time.Now().Add(1 * time.Hour)

	sessionID := uuid.NewString()

	sess, err := session.Get(defaultSessionIDKey, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get session")
	}

	sess.Options = &sessions.Options{
		Domain: "u.isucon.local",
		MaxAge: int(60000),
		Path:   "/",
	}
	sess.Values[defaultSessionIDKey] = sessionID
	sess.Values[defaultUserIDKey] = userModel.ID
	sess.Values[defaultUsernameKey] = userModel.Name
	sess.Values[defaultSessionExpiresKey] = sessionEndAt.Unix()

	if err := sess.Save(c.Request(), c.Response()); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to save session: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

// ユーザ詳細API
// GET /api/user/:username
func getUserHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	username := c.Param("username")

	// Read-only: no transaction; users are insert-only and cached.
	userModel, err := getUserModelByName(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	user, err := fillUserResponse(ctx, dbConn, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	return c.JSON(http.StatusOK, user)
}

func verifyUserSession(c echo.Context) error {
	sess, err := session.Get(defaultSessionIDKey, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get session")
	}

	sessionExpires, ok := sess.Values[defaultSessionExpiresKey]
	if !ok {
		return echo.NewHTTPError(http.StatusForbidden, "failed to get EXPIRES value from session")
	}

	_, ok = sess.Values[defaultUserIDKey].(int64)
	if !ok {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get USERID value from session")
	}

	now := time.Now()
	if now.Unix() > sessionExpires.(int64) {
		return echo.NewHTTPError(http.StatusUnauthorized, "session has expired")
	}

	return nil
}

// iconHashCache caches user id -> hex sha256 of the user's current icon image.
// fillUserResponse is on nearly every hot path, and previously fetched the full
// image BLOB from MySQL and ran sha256 over it on every call (35% of the app's
// CPU in the pprof profile, plus ~64k `SELECT image FROM icons` queries per run).
// There is a single app process and the only icon writer is postIconHandler, which
// Stores the new hash after commit; readers use LoadOrStore so a reader that read
// the old image before the commit can never overwrite the writer's newer value.
// initializeHandler clears it (users/icons are truncated and ids reused).
var iconHashCache sync.Map

func computeIconHash(image []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(image))
}

// dbq is the read interface shared by *sqlx.Tx and *sqlx.DB, so read-only handlers can call the
// fill* helpers without opening a transaction (each helper only issues single-statement reads).
type dbq interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

// ctxGetter is satisfied by both *sqlx.Tx and *sqlx.DB.
type ctxGetter interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

func getIconHash(ctx context.Context, tx ctxGetter, userID int64) (string, error) {
	if v, ok := iconHashCache.Load(userID); ok {
		return v.(string), nil
	}
	image, err := getIconImageWith(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	actual, _ := iconHashCache.LoadOrStore(userID, computeIconHash(image))
	return actual.(string), nil
}

// iconImageCache caches user id -> current icon image bytes (nil-length/absent icon row is
// stored as a nil []byte meaning "use the fallback image"). Same coherence rules as iconHashCache:
// readers LoadOrStore, postIconHandler Stores after commit, initialize clears.
var (
	iconImageCache  sync.Map // int64 -> []byte
	userByNameCache sync.Map // string -> UserModel (users are insert-only)

	fallbackImageOnce  sync.Once
	fallbackImageBytes []byte
	fallbackImageErr   error
)

func getFallbackImage() ([]byte, error) {
	fallbackImageOnce.Do(func() {
		fallbackImageBytes, fallbackImageErr = os.ReadFile(fallbackImage)
	})
	return fallbackImageBytes, fallbackImageErr
}

// getIconImageWith returns the user's icon bytes (or the fallback image), using the cache when possible.
func getIconImageWith(ctx context.Context, q ctxGetter, userID int64) ([]byte, error) {
	if v, ok := iconImageCache.Load(userID); ok {
		if img := v.([]byte); img != nil {
			return img, nil
		}
		return getFallbackImage()
	}
	var image []byte
	if err := q.GetContext(ctx, &image, "SELECT image FROM icons WHERE user_id = ?", userID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		image = nil
	}
	actual, _ := iconImageCache.LoadOrStore(userID, image)
	if img := actual.([]byte); img != nil {
		return img, nil
	}
	return getFallbackImage()
}

func getIconImage(ctx context.Context, userID int64) ([]byte, error) {
	return getIconImageWith(ctx, dbConn, userID)
}

func getUserModelByName(ctx context.Context, name string) (UserModel, error) {
	if v, ok := userByNameCache.Load(name); ok {
		return v.(UserModel), nil
	}
	var user UserModel
	if err := dbConn.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", name); err != nil {
		return UserModel{}, err
	}
	userByNameCache.Store(name, user)
	return user, nil
}

// userModelCache / themeModelCache: users and themes are insert-only in this app
// (no UPDATE/DELETE anywhere; register is the sole writer), so once a row has been
// read it can never change. This removed ~90k `SELECT * FROM users WHERE id=?` and
// ~90k `SELECT * FROM themes WHERE user_id=?` per benchmark run. Only successful
// reads are cached (ErrNoRows is never cached); cleared by initializeHandler, which
// truncates the tables and reuses ids.
var (
	userModelCache  sync.Map // int64 -> UserModel
	themeModelCache sync.Map // int64 (user id) -> ThemeModel
)

func clearUserCaches() {
	for _, m := range []*sync.Map{&userModelCache, &themeModelCache, &iconHashCache, &iconImageCache, &userByNameCache, &livestreamModelCache, &livestreamTagsCache} {
		m.Range(func(k, _ any) bool { m.Delete(k); return true })
	}
}

func getUserModelByID(ctx context.Context, tx dbq, userID int64) (UserModel, error) {
	if v, ok := userModelCache.Load(userID); ok {
		return v.(UserModel), nil
	}
	userModel := UserModel{}
	if err := tx.GetContext(ctx, &userModel, "SELECT * FROM users WHERE id = ?", userID); err != nil {
		return UserModel{}, err
	}
	userModelCache.Store(userID, userModel)
	return userModel, nil
}

func getThemeModel(ctx context.Context, tx dbq, userID int64) (ThemeModel, error) {
	if v, ok := themeModelCache.Load(userID); ok {
		return v.(ThemeModel), nil
	}
	themeModel := ThemeModel{}
	if err := tx.GetContext(ctx, &themeModel, "SELECT * FROM themes WHERE user_id = ?", userID); err != nil {
		return ThemeModel{}, err
	}
	themeModelCache.Store(userID, themeModel)
	return themeModel, nil
}

func fillUserResponse(ctx context.Context, tx dbq, userModel UserModel) (User, error) {
	themeModel, err := getThemeModel(ctx, tx, userModel.ID)
	if err != nil {
		return User{}, err
	}

	iconHash, err := getIconHash(ctx, tx, userModel.ID)
	if err != nil {
		return User{}, err
	}

	user := User{
		ID:          userModel.ID,
		Name:        userModel.Name,
		DisplayName: userModel.DisplayName,
		Description: userModel.Description,
		Theme: Theme{
			ID:       themeModel.ID,
			DarkMode: themeModel.DarkMode,
		},
		IconHash: iconHash,
	}

	return user, nil
}
