package main

// ISUCON的な参考: https://github.com/isucon/isucon12-qualify/blob/main/webapp/go/isuports.go#L336
// sqlx的な参考: https://jmoiron.github.io/sqlx/

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"github.com/gorilla/sessions"
	"github.com/kaz/pprotein/integration/standalone"
	"github.com/labstack/echo-contrib/session"
	echolog "github.com/labstack/gommon/log"
)

const (
	listenPort                     = 8080
	powerDNSSubdomainAddressEnvKey = "ISUCON13_POWERDNS_SUBDOMAIN_ADDRESS"
)

var (
	powerDNSSubdomainAddress string
	dbConn                   *sqlx.DB
	secret                   = []byte("isucon13_session_cookiestore_defaultsecret")
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	if secretKey, ok := os.LookupEnv("ISUCON13_SESSION_SECRETKEY"); ok {
		secret = []byte(secretKey)
	}
}

type InitializeResponse struct {
	Language string `json:"language"`
}

func connectDB(logger echo.Logger) (*sqlx.DB, error) {
	const (
		networkTypeEnvKey = "ISUCON13_MYSQL_DIALCONFIG_NET"
		addrEnvKey        = "ISUCON13_MYSQL_DIALCONFIG_ADDRESS"
		portEnvKey        = "ISUCON13_MYSQL_DIALCONFIG_PORT"
		userEnvKey        = "ISUCON13_MYSQL_DIALCONFIG_USER"
		passwordEnvKey    = "ISUCON13_MYSQL_DIALCONFIG_PASSWORD"
		dbNameEnvKey      = "ISUCON13_MYSQL_DIALCONFIG_DATABASE"
		parseTimeEnvKey   = "ISUCON13_MYSQL_DIALCONFIG_PARSETIME"
	)

	conf := mysql.NewConfig()

	// 環境変数がセットされていなかった場合でも一旦動かせるように、デフォルト値を入れておく
	// この挙動を変更して、エラーを出すようにしてもいいかもしれない
	conf.Net = "tcp"
	conf.Addr = net.JoinHostPort("127.0.0.1", "3306")
	conf.User = "isucon"
	conf.Passwd = "isucon"
	conf.DBName = "isupipe"
	conf.ParseTime = true
	// Without this every query was Prepare + Execute + Close (3 round trips; ~250k Prepares per
	// bench window, 15s of MySQL time). Client-side interpolation makes it a single round trip.
	conf.InterpolateParams = true

	if v, ok := os.LookupEnv(networkTypeEnvKey); ok {
		conf.Net = v
	}
	if addr, ok := os.LookupEnv(addrEnvKey); ok {
		if port, ok2 := os.LookupEnv(portEnvKey); ok2 {
			conf.Addr = net.JoinHostPort(addr, port)
		} else {
			conf.Addr = net.JoinHostPort(addr, "3306")
		}
	}
	if v, ok := os.LookupEnv(userEnvKey); ok {
		conf.User = v
	}
	if v, ok := os.LookupEnv(passwordEnvKey); ok {
		conf.Passwd = v
	}
	if v, ok := os.LookupEnv(dbNameEnvKey); ok {
		conf.DBName = v
	}
	if v, ok := os.LookupEnv(parseTimeEnvKey); ok {
		parseTime, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("failed to parse environment variable '%s' as bool: %+v", parseTimeEnvKey, err)
		}
		conf.ParseTime = parseTime
	}

	db, err := sqlx.Open("mysql", conf.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(50)

	if err := db.Ping(); err != nil {
		return nil, err
	}

	return db, nil
}

func initializeHandler(c echo.Context) error {
	if out, err := exec.Command("../sql/init.sh").CombinedOutput(); err != nil {
		c.Logger().Warnf("init.sh failed with err=%s", string(out))
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to initialize: "+err.Error())
	}

	clearUserCaches()

	// init.sh の初期データ投入が生むログも消すため、必ず init.sh の後・収集の前に行う
	rotateLogs()

	collectPprotein()

	c.Request().Header.Add("Content-Type", "application/json;charset=utf-8")
	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "golang",
	})
}

// rotateLogs truncates the nginx access log (this host) and the MySQL slow log (DB host)
// so each benchmark run starts from empty logs; otherwise pprotein's httplog/slowlog
// artifacts accumulate every previous run. Best-effort: never fails /api/initialize.
func rotateLogs() {
	if os.Getenv("LOG_ROTATE_DISABLED") == "1" {
		return
	}

	run := func(name string, args ...string) {
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			log.Printf("log rotation: %s %v failed: %v: %s", name, args, err, string(out))
		}
	}

	run("sudo", "truncate", "-s", "0", "/var/log/nginx/access.log")

	const slowLog = "/var/log/mysql/mysql-slow.log"
	dbHost := os.Getenv("ISUCON13_MYSQL_DIALCONFIG_ADDRESS")
	if dbHost == "" || dbHost == "127.0.0.1" || dbHost == "localhost" {
		run("sudo", "truncate", "-s", "0", slowLog)
		return
	}
	dnsHost := os.Getenv("ISUCON13_DNS_HOST")
	if dnsHost == "" {
		dnsHost = "192.168.0.13"
	}
	for _, h := range []string{dbHost, dnsHost} {
		run("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=3",
			"isucon@"+h, "sudo truncate -s 0 "+slowLog)
	}
}

func collectPprotein() {
	if os.Getenv("PPROTEIN_COLLECT_DISABLED") == "1" {
		return
	}

	url := os.Getenv("PPROTEIN_COLLECT_URL")
	if url == "" {
		url = "http://s4:9000/api/group/collect"
	}

	go func() {
		resp, err := http.Get(url)
		if err != nil {
			log.Printf("failed to communicate with pprotein: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("pprotein collection returned error status=%d", resp.StatusCode)
		}
	}()
}

func main() {
	go standalone.Integrate(":8888")
	e := echo.New()
	e.Debug = false // true makes echo indent every JSON response (5% of app CPU in pprof)
	e.Logger.SetLevel(echolog.WARN) // per-request access logging lives in nginx (LTSV); keep app log for warnings/errors only
	// echo access logger removed: nginx LTSV access.log already records every request (with reqtime/apptime); this was ~4% of s1 CPU in journald plus formatting
	cookieStore := sessions.NewCookieStore(secret)
	cookieStore.Options.Domain = "*.u.isucon.local"
	e.Use(session.Middleware(newMemoStore(cookieStore))) // memoises successful cookie decodes (see session_cache.go)
	// e.Use(middleware.Recover())

	// 初期化
	e.POST("/api/initialize", initializeHandler)

	// top
	e.GET("/api/tag", getTagHandler)
	e.GET("/api/user/:username/theme", getStreamerThemeHandler)

	// livestream
	// reserve livestream
	e.POST("/api/livestream/reservation", reserveLivestreamHandler)
	// list livestream
	e.GET("/api/livestream/search", searchLivestreamsHandler)
	e.GET("/api/livestream", getMyLivestreamsHandler)
	e.GET("/api/user/:username/livestream", getUserLivestreamsHandler)
	// get livestream
	e.GET("/api/livestream/:livestream_id", getLivestreamHandler)
	// get polling livecomment timeline
	e.GET("/api/livestream/:livestream_id/livecomment", getLivecommentsHandler)
	// ライブコメント投稿
	e.POST("/api/livestream/:livestream_id/livecomment", postLivecommentHandler)
	e.POST("/api/livestream/:livestream_id/reaction", postReactionHandler)
	e.GET("/api/livestream/:livestream_id/reaction", getReactionsHandler)

	// (配信者向け)ライブコメントの報告一覧取得API
	e.GET("/api/livestream/:livestream_id/report", getLivecommentReportsHandler)
	e.GET("/api/livestream/:livestream_id/ngwords", getNgwords)
	// ライブコメント報告
	e.POST("/api/livestream/:livestream_id/livecomment/:livecomment_id/report", reportLivecommentHandler)
	// 配信者によるモデレーション (NGワード登録)
	e.POST("/api/livestream/:livestream_id/moderate", moderateHandler)

	// livestream_viewersにINSERTするため必要
	// ユーザ視聴開始 (viewer)
	e.POST("/api/livestream/:livestream_id/enter", enterLivestreamHandler)
	// ユーザ視聴終了 (viewer)
	e.DELETE("/api/livestream/:livestream_id/exit", exitLivestreamHandler)

	// user
	e.POST("/api/register", registerHandler)
	e.POST("/api/login", loginHandler)
	e.GET("/api/user/me", getMeHandler)
	// フロントエンドで、配信予約のコラボレーターを指定する際に必要
	e.GET("/api/user/:username", getUserHandler)
	e.GET("/api/user/:username/statistics", getUserStatisticsHandler)
	e.GET("/api/user/:username/icon", getIconHandler)
	e.POST("/api/icon", postIconHandler)

	// stats
	// ライブ配信統計情報
	e.GET("/api/livestream/:livestream_id/statistics", getLivestreamStatisticsHandler)

	// 課金情報
	e.GET("/api/payment", GetPaymentResult)

	e.HTTPErrorHandler = errorResponseHandler

	// DB接続
	conn, err := connectDB(e.Logger)
	if err != nil {
		e.Logger.Errorf("failed to connect db: %v", err)
		os.Exit(1)
	}
	defer conn.Close()
	dbConn = conn

	subdomainAddr, ok := os.LookupEnv(powerDNSSubdomainAddressEnvKey)
	if !ok {
		e.Logger.Errorf("environ %s must be provided", powerDNSSubdomainAddressEnvKey)
		os.Exit(1)
	}
	powerDNSSubdomainAddress = subdomainAddr

	// HTTPサーバ起動
	listenAddr := net.JoinHostPort("", strconv.Itoa(listenPort))
	if err := e.Start(listenAddr); err != nil {
		e.Logger.Errorf("failed to start HTTP server: %v", err)
		os.Exit(1)
	}
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func errorResponseHandler(err error, c echo.Context) {
	c.Logger().Errorf("error at %s: %+v", c.Path(), err)
	if he, ok := err.(*echo.HTTPError); ok {
		if e := c.JSON(he.Code, &ErrorResponse{Error: err.Error()}); e != nil {
			c.Logger().Errorf("%+v", e)
		}
		return
	}

	if e := c.JSON(http.StatusInternalServerError, &ErrorResponse{Error: err.Error()}); e != nil {
		c.Logger().Errorf("%+v", e)
	}
}
