package main

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

func TestMigrationTableInventoryMatchesSQLiteSchema(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	want := slices.Clone(tableOrder)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("migration table inventory = %v, schema tables = %v", want, got)
	}
}

func TestLivePostgresMigrationAndQueries(t *testing.T) {
	dsn := os.Getenv("DUCKWAY_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("DUCKWAY_TEST_POSTGRES_URL is not set")
	}
	t.Setenv("DUCKWAY_DATABASE_URL", dsn)
	source, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	clientQ := queries.NewClientQueries(source)
	if err := clientQ.Create(&models.Client{ID: "pg-client", ShortID: "pgtest", Name: "Postgres Test", TokenHash: "pg-hash", CanaryEnabled: true}); err != nil {
		t.Fatal(err)
	}
	keyQ := queries.NewAPIKeyQueries(source)
	if err := keyQ.Create(&models.APIKey{ID: "pg-key", ServiceID: "svc-openai-default", Name: "Postgres Key", KeyEncrypted: "ciphertext", RefreshToken: "refresh-ciphertext"}); err != nil {
		t.Fatal(err)
	}
	logQ := queries.NewRequestLogQueries(source)
	logID, err := logQ.LogWithReturn("pg-client", "", "openai", "POST", "/responses", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := logQ.StoreDetail(&queries.RequestLogDetail{LogID: logID, RequestBody: "request", ResponseBody: "response", Truncated: true}); err != nil {
		t.Fatal(err)
	}

	target, err := database.OpenPostgresFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := migrate(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), source, target); err == nil {
		t.Fatal("second migration unexpectedly replaced a non-empty PostgreSQL target")
	}

	gotClient, err := queries.NewClientQueries(target).GetByID("pg-client")
	if err != nil || !gotClient.CanaryEnabled {
		t.Fatalf("migrated client = %#v, err=%v", gotClient, err)
	}
	gotKey, err := queries.NewAPIKeyQueries(target).GetByID("pg-key")
	if err != nil || gotKey.KeyEncrypted != "ciphertext" || gotKey.RefreshToken != "refresh-ciphertext" {
		t.Fatalf("migrated key = %#v, err=%v", gotKey, err)
	}
	if _, err := queries.NewRequestLogQueries(target).LogWithReturn("pg-client", "", "openai", "POST", "/responses", 201); err != nil {
		t.Fatalf("PostgreSQL RETURNING query: %v", err)
	}
	if _, err := queries.NewApprovalQueries(target).MarkExpiredAsIgnored(60); err != nil {
		t.Fatalf("PostgreSQL datetime modifier query: %v", err)
	}
}
