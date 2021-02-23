package pg

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"testing"

	"github.com/docker/go-connections/nat"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/quintans/eventstore/store/postgresql"
	"github.com/quintans/faults"
	testcontainers "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	dbConfig = postgresql.DBConfig{
		Database: "eventstore",
		Host:     "localhost",
		Port:     5432,
		Username: "postgres",
		Password: "postgres",
	}
)

func TestMain(m *testing.M) {
	tearDown, err := setup()
	if err != nil {
		log.Fatal(err)
	}

	// test run
	var code int
	func() {
		defer tearDown()
		code = m.Run()
	}()

	os.Exit(code)
}

func setup() (func(), error) {
	ctx := context.Background()

	tearDown, err := bootstrapDbContainer(ctx)
	if err != nil {
		return nil, err
	}

	return tearDown, nil
}

func bootstrapDbContainer(ctx context.Context) (func(), error) {
	tcpPort := strconv.Itoa(dbConfig.Port)
	natPort := nat.Port(tcpPort)

	req := testcontainers.ContainerRequest{
		Image:        "postgres:12.3",
		ExposedPorts: []string{tcpPort + "/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     dbConfig.Username,
			"POSTGRES_PASSWORD": dbConfig.Password,
			"POSTGRES_DB":       dbConfig.Database,
		},
		WaitingFor: wait.ForListeningPort(natPort),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, faults.Wrap(err)
	}

	tearDown := func() {
		container.Terminate(ctx)
	}

	ip, err := container.Host(ctx)
	if err != nil {
		tearDown()
		return nil, faults.Wrap(err)
	}
	port, err := container.MappedPort(ctx, natPort)
	if err != nil {
		tearDown()
		return nil, faults.Wrap(err)
	}

	dbConfig.Host = ip
	dbConfig.Port = port.Int()

	err = dbSchema(dbConfig)
	if err != nil {
		tearDown()
		return nil, faults.Wrap(err)
	}

	return tearDown, nil
}

func dbSchema(dbConfig postgresql.DBConfig) error {
	dbURL := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", dbConfig.Username, dbConfig.Password, dbConfig.Host, dbConfig.Port, dbConfig.Database)
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		return faults.Wrap(err)
	}
	defer db.Close()

	db.MustExec(`
	CREATE TABLE IF NOT EXISTS events(
		id VARCHAR (50) PRIMARY KEY,
		aggregate_id VARCHAR (50) NOT NULL,
		aggregate_id_hash INTEGER NOT NULL,
		aggregate_version INTEGER NOT NULL,
		aggregate_type VARCHAR (50) NOT NULL,
		kind VARCHAR (50) NOT NULL,
		body bytea NOT NULL,
		idempotency_key VARCHAR (50),
		labels JSONB NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT NOW()::TIMESTAMP
	);
	CREATE INDEX evt_agg_id_idx ON events (aggregate_id);
	CREATE UNIQUE INDEX evt_agg_id_ver_uk ON events (aggregate_id, aggregate_version);
	CREATE UNIQUE INDEX evt_agg_idempot_uk ON events (aggregate_type, idempotency_key);
	CREATE INDEX evt_labels_idx ON events USING GIN (labels jsonb_path_ops);

	CREATE TABLE IF NOT EXISTS snapshots(
		id VARCHAR (50) PRIMARY KEY,
		aggregate_id VARCHAR (50) NOT NULL,
		aggregate_version INTEGER NOT NULL,
		aggregate_type VARCHAR (50) NOT NULL,
		body bytea NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT NOW()::TIMESTAMP,
		FOREIGN KEY (id) REFERENCES events (id)
	);
	CREATE INDEX snap_agg_id_idx ON snapshots (aggregate_id);
	
	CREATE OR REPLACE FUNCTION notify_event() RETURNS TRIGGER AS $FN$
		DECLARE 
			notification json;
		BEGIN
			notification = row_to_json(NEW);
			PERFORM pg_notify('events_channel', notification::text);
			
			-- Result is ignored since this is an AFTER trigger
			RETURN NULL; 
		END;
	$FN$ LANGUAGE plpgsql;
	
	CREATE TRIGGER events_notify_event
	AFTER INSERT ON events
		FOR EACH ROW EXECUTE PROCEDURE notify_event();
	`)

	return nil
}
