package db

import (
	"net/url"
	"testing"

	"recallweave/internal/config"
)

func TestDatabaseDSNEscapesCredentialsAndIPv6(t *testing.T) {
	cfg := config.DatabaseConfig{Host: "::1", Port: 5434, UserName: "test@user", Password: "a@:/?#% secret", DBName: "recallweave"}
	u, err := url.Parse(databaseDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.User.Username() != cfg.UserName || password != cfg.Password || u.Hostname() != cfg.Host || u.Port() != "5434" || u.Path != "/recallweave" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("database DSN changed connection parameters")
	}
}
