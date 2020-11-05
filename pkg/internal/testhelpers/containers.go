// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license

package testhelpers

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	defaultDB       = "postgres"
	connectTemplate = "postgres://%s:password@%s:%d/%s"

	postgresUser    = "postgres"
	promUser        = "prom"
	emptyPromConfig = "global:\n  scrape_interval: 10s"

	Superuser   = true
	NoSuperuser = false
)

type ExtensionState int

const (
	timescaleBit  = 1 << iota
	promscaleBit  = 1 << iota
	timescale2Bit = 1 << iota
	multinodeBit  = 1 << iota
)

const (
	VanillaPostgres        ExtensionState = 0
	Timescale1             ExtensionState = timescaleBit
	Timescale1AndPromscale ExtensionState = timescaleBit | promscaleBit
	Timescale2             ExtensionState = timescaleBit | timescale2Bit
	Timescale2AndPromscale ExtensionState = timescaleBit | timescale2Bit | promscaleBit
	Multinode              ExtensionState = timescaleBit | timescale2Bit | multinodeBit
	MultinodeAndPromscale  ExtensionState = timescaleBit | timescale2Bit | multinodeBit | promscaleBit
)

func (e *ExtensionState) UseTimescaleDB() {
	*e |= timescaleBit
}

func (e *ExtensionState) UsePromscale() {
	*e |= timescaleBit | promscaleBit
}

func (e *ExtensionState) UseTimescale2() {
	*e |= timescaleBit | timescale2Bit
}

func (e *ExtensionState) UseMultinode() {
	*e |= timescaleBit | timescale2Bit | multinodeBit
}

func (e ExtensionState) usesTimescaleDB() bool {
	switch e {
	case VanillaPostgres:
		return false
	default:
		return true
	}
}

func (e ExtensionState) usesMultinode() bool {
	switch e {
	case Multinode:
		return true
	case MultinodeAndPromscale:
		return true
	default:
		return false
	}
}

func (e ExtensionState) usesPromscale() bool {
	switch e {
	case Timescale1AndPromscale:
		return true
	case Timescale2AndPromscale:
		return true
	case MultinodeAndPromscale:
		return true
	default:
		return false
	}
}

var (
	PromHost          = "localhost"
	PromPort nat.Port = "9090/tcp"

	pgHost          = "localhost"
	pgPort nat.Port = "5432/tcp"
)

type SuperuserStatus = bool

func PgConnectURL(dbName string, superuser SuperuserStatus) string {
	user := postgresUser
	if !superuser {
		user = promUser
	}
	return fmt.Sprintf(connectTemplate, user, pgHost, pgPort.Int(), dbName)
}

// WithDB establishes a database for testing and calls the callback
func WithDB(t testing.TB, DBName string, superuser SuperuserStatus, f func(db *pgxpool.Pool, t testing.TB, connectString string)) {
	db, err := DbSetup(DBName, superuser)
	if err != nil {
		t.Fatal(err)
		return
	}
	defer db.Close()
	f(db, t, PgConnectURL(DBName, superuser))
}

func GetReadOnlyConnection(t testing.TB, DBName string) *pgxpool.Pool {
	dbPool, err := pgxpool.Connect(context.Background(), PgConnectURL(DBName, NoSuperuser))
	if err != nil {
		t.Fatal(err)
	}

	_, err = dbPool.Exec(context.Background(), "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY")
	if err != nil {
		t.Fatal(err)
	}

	return dbPool
}

func DbSetup(DBName string, superuser SuperuserStatus) (*pgxpool.Pool, error) {
	db, err := pgx.Connect(context.Background(), PgConnectURL(defaultDB, Superuser))
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s", DBName))
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(context.Background(), fmt.Sprintf("CREATE DATABASE %s OWNER %s", DBName, promUser))
	if err != nil {
		return nil, err
	}

	err = db.Close(context.Background())
	if err != nil {
		return nil, err
	}

	//some docker images may have timescaledb installed in a template, which we don't always want
	dbDrop, err := pgx.Connect(context.Background(), PgConnectURL(DBName, Superuser))
	if err != nil {
		return nil, err
	}
	if _, err = dbDrop.Exec(context.Background(), "DROP EXTENSION IF EXISTS timescaledb"); err != nil {
		return nil, err
	}
	if err = dbDrop.Close(context.Background()); err != nil {
		return nil, err
	}

	dbPool, err := pgxpool.Connect(context.Background(), PgConnectURL(DBName, superuser))
	if err != nil {
		return nil, err
	}
	return dbPool, nil
}

type stdoutLogConsumer struct {
}

func (s stdoutLogConsumer) Accept(l testcontainers.Log) {
	if l.LogType == testcontainers.StderrLog {
		fmt.Print(l.LogType, " ", string(l.Content))
	} else {
		fmt.Print(string(l.Content))
	}
}

// StartPGContainer starts a postgreSQL container for use in testing
func StartPGContainer(
	ctx context.Context,
	extensionState ExtensionState,
	testDataDir string,
	printLogs bool,
) (testcontainers.Container, io.Closer, error) {
	var image string
	switch extensionState {
	case MultinodeAndPromscale:
		image = "timescaledev/promscale-extension:2.0.0-rc2-pg12"
	case Multinode:
		image = "timescale/timescaledb:2.0.0-rc2-pg12"
	case Timescale2AndPromscale:
		image = "timescaledev/promscale-extension:2.0.0-rc2-pg12"
	case Timescale2:
		image = "timescale/timescaledb:2.0.0-rc2-pg12"
	case Timescale1AndPromscale:
		image = "timescaledev/promscale-extension:latest-pg12"
	case Timescale1:
		image = "timescale/timescaledb:latest-pg12"
	case VanillaPostgres:
		image = "postgres:12"
	}

	return StartPGContainerWithImage(ctx, image, testDataDir, "", printLogs, extensionState)
}

func StartPGContainerWithImage(ctx context.Context,
	image string,
	testDataDir string,
	dataDir string,
	printLogs bool,
	extensionState ExtensionState,
) (testcontainers.Container, io.Closer, error) {

	multinode := extensionState.usesMultinode()

	c := CloseAll{}
	var networks []string
	if multinode {
		networkName := "promscale-network"
		networkRequest := testcontainers.GenericNetworkRequest{
			NetworkRequest: testcontainers.NetworkRequest{
				Driver:     "bridge",
				Name:       networkName,
				Attachable: true,
			},
		}
		network, err := testcontainers.GenericNetwork(context.Background(), networkRequest)
		if err != nil {
			return nil, nil, err
		}
		c.Append(func() { network.Remove(context.Background()) })
		networks = []string{networkName}
	}

	startContainer := func(containerPort nat.Port) (testcontainers.Container, error) {
		// we map the access node to port 5432 and the others to other ports
		isDataNode := containerPort.Port() != "5432"

		req := testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{string(containerPort)},
			WaitingFor: wait.ForSQL(containerPort, "pgx", func(port nat.Port) string {
				return "dbname=postgres password=password user=postgres host=127.0.0.1 port=" + port.Port()
			}).Timeout(60 * time.Second),
			Env: map[string]string{
				"POSTGRES_PASSWORD": "password",
			},
			Networks:       networks,
			NetworkAliases: map[string][]string{"promscale-network": {"db" + containerPort.Port()}},
			SkipReaper:     false, /* switch to true not to kill docker container */
		}
		req.Cmd = []string{
			"-c", "max_connections=100",
			"-c", "port=" + containerPort.Port(),
			"-i",
		}

		if extensionState.usesTimescaleDB() {
			req.Cmd = append(req.Cmd,
				"-c", "shared_preload_libraries=timescaledb",
				"-c", "timescaledb.max_background_workers=0",
			)
		}

		if extensionState.usesPromscale() {
			req.Cmd = append(req.Cmd,
				"-c", "local_preload_libraries=pgextwlist",
				//timescale_prometheus_extra included for upgrade tests with old extension name
				"-c", "extwlist.extensions=promscale,timescaledb,timescale_prometheus_extra",
			)
		}

		req.BindMounts = make(map[string]string)
		if testDataDir != "" {
			req.BindMounts[testDataDir] = "/testdata"
		}
		if dataDir != "" {
			req.BindMounts[dataDir] = "/var/lib/postgresql/data"
		}

		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          false,
		})
		if err != nil {
			return nil, err
		}

		if printLogs {
			container.FollowOutput(stdoutLogConsumer{})
		}

		if printLogs {
			err = container.StartLogProducer(context.Background())
			if err != nil {
				fmt.Println("Error setting up logger", err)
				os.Exit(1)
			}
		}

		err = container.Start(context.Background())
		if err != nil {
			return nil, err
		}

		c.Append(func() {
			if printLogs {
				_ = container.StopLogProducer()
			}

			_ = container.Terminate(ctx)
		})

		// we map the access node to port 5432 and the others to other ports
		if isDataNode {
			// trust all incoming connections on the datanodes so we can add them to
			// the cluster without too much hassle. We'll reload the pg_hba.conf later
			// NOTE: the setting must be prepended to pg_hba.conf so it overrides
			//       the latter, stricter, settings
			code, err := container.Exec(context.Background(), []string{
				"bash",
				"-c",
				"echo -e \"host all all 0.0.0.0/0 trust\n$(cat /var/lib/postgresql/data/pg_hba.conf)\"" +
					"> /var/lib/postgresql/data/pg_hba.conf"})
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, fmt.Errorf("docker exec error. exit code: %d", code)
			}
		}

		pgHost, err = container.Host(ctx)
		if err != nil {
			return nil, err
		}

		pgPort, err = container.MappedPort(ctx, containerPort)
		if err != nil {
			return nil, err
		}

		db, err := pgx.Connect(context.Background(), PgConnectURL(defaultDB, Superuser))
		if err != nil {
			return nil, err
		}

		_, err = db.Exec(context.Background(), fmt.Sprintf("CREATE USER %s WITH NOSUPERUSER CREATEROLE PASSWORD 'password'", promUser))
		if err != nil {
			//ignore duplicate errors
			pgErr, ok := err.(*pgconn.PgError)
			if !ok || pgErr.Code != "42710" {
				return nil, err
			}
		}

		if isDataNode {
			// reload the pg_hba.conf settings we changed above
			_, err = db.Exec(context.Background(), "SELECT pg_reload_conf()")
			if err != nil {
				return nil, err
			}
		}

		err = db.Close(context.Background())
		if err != nil {
			return nil, err
		}

		return container, nil
	}

	if multinode {
		containerPort := nat.Port("5433/tcp")
		_, err := startContainer(containerPort)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		containerPort = nat.Port("5434/tcp")
		_, err = startContainer(containerPort)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
	}

	containerPort := nat.Port("5432/tcp")
	container, err := startContainer(containerPort)
	if err != nil {
		c.Close()
		return nil, nil, err
	}

	if multinode {
		db, err := pgx.Connect(context.Background(), PgConnectURL(defaultDB, Superuser))
		if err != nil {
			c.Close()
			return nil, nil, err
		}

		_, err = db.Exec(context.Background(), "SELECT add_data_node('dn0', host => 'db5433', port => 5433);")
		if err != nil {
			c.Close()
			return nil, nil, err
		}

		_, err = db.Exec(context.Background(), "SELECT add_data_node('dn1', host => 'db5434', port => 5434);")
		if err != nil {
			c.Close()
			return nil, nil, err
		}

		err = db.Close(context.Background())
		if err != nil {
			c.Close()
			return nil, nil, err
		}
	}
	return container, c, nil
}

type CloseAll struct {
	toClose []func()
}

func (c *CloseAll) Append(closer func()) {
	c.toClose = append(c.toClose, closer)
}

func (c CloseAll) Close() error {
	for i := len(c.toClose) - 1; i >= 0; i-- {
		c.toClose[i]()
	}
	return nil
}

// StartPromContainer starts a Prometheus container for use in testing
func StartPromContainer(storagePath string, ctx context.Context) (testcontainers.Container, error) {
	// Set the storage directories permissions so Prometheus can write to them.
	err := os.Chmod(storagePath, 0777)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(storagePath, "wal"), 0777); err != nil {
		return nil, err
	}

	promConfigFile := filepath.Join(storagePath, "prometheus.yml")
	err = ioutil.WriteFile(promConfigFile, []byte(emptyPromConfig), 0777)
	if err != nil {
		return nil, err
	}
	prometheusPort := nat.Port("9090/tcp")
	req := testcontainers.ContainerRequest{
		Image:        "prom/prometheus",
		ExposedPorts: []string{string(prometheusPort)},
		WaitingFor:   wait.ForListeningPort(prometheusPort),
		BindMounts: map[string]string{
			storagePath:    "/prometheus",
			promConfigFile: "/etc/prometheus/prometheus.yml",
		},
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, err
	}
	PromHost, err = container.Host(ctx)
	if err != nil {
		return nil, err
	}

	PromPort, err = container.MappedPort(ctx, prometheusPort)
	if err != nil {
		return nil, err
	}

	return container, nil
}

var ConnectorPort = nat.Port("9201/tcp")

func StartConnectorWithImage(ctx context.Context, image string, printLogs bool, dbname string) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{string(ConnectorPort)},
		WaitingFor:   wait.ForHTTP("/write").WithPort(ConnectorPort).WithAllowInsecure(true),
		SkipReaper:   false, /* switch to true not to kill docker container */
		Cmd: []string{
			"-db-host", "172.17.0.1", // IP refering to the docker's host network
			"-db-port", pgPort.Port(),
			"-db-user", promUser,
			"-db-password", "password",
			"-db-name", dbname,
			"-db-ssl-mode", "prefer",
			"-web-listen-address", "0.0.0.0:" + ConnectorPort.Port(),
		},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          false,
	})
	if err != nil {
		return nil, err
	}

	if printLogs {
		container.FollowOutput(stdoutLogConsumer{})
	}

	if printLogs {
		err = container.StartLogProducer(context.Background())
		if err != nil {
			fmt.Println("Error setting up logger", err)
			os.Exit(1)
		}
	}

	err = container.Start(context.Background())
	if err != nil {
		return nil, err
	}

	return container, nil
}

func StopContainer(ctx context.Context, container testcontainers.Container, printLogs bool) {
	if printLogs {
		_ = container.StopLogProducer()
	}

	_ = container.Terminate(ctx)
}

// TempDir returns a temp directory for tests
func TempDir(name string) (string, error) {
	tmpDir := ""

	if runtime.GOOS == "darwin" {
		// Docker on Mac lacks access to default os tmp dir - "/var/folders/random_number"
		// so switch to cross-user tmp dir
		tmpDir = "/tmp"
	}
	return ioutil.TempDir(tmpDir, name)
}
