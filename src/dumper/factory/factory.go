package factory

import (
	"fmt"

	"backup-operator/dumper"
	"backup-operator/dumper/mongo"
	"backup-operator/dumper/mysql"
	"backup-operator/dumper/postgres"
	"backup-operator/dumper/redis"

	"github.com/go-logr/logr"
)

const (
	TypePostgres = "postgres"
	TypeMySQL    = "mysql"
	TypeMariaDB  = "mariadb"
	TypeMongo    = "mongo"
	TypeRedis    = "redis"
)

// NewDumper creates the right Dumper for the given db-type label.
// Add new database types by implementing dumper.Dumper and registering here.
// Never branch on db type outside this factory.
func NewDumper(dbType string, cfg dumper.Config, logger logr.Logger) (dumper.Dumper, error) {
	switch dbType {
	case TypePostgres:
		return postgres.New(cfg, logger.WithName("postgres")), nil
	case TypeMySQL, TypeMariaDB:
		// MariaDB speaks the MySQL wire protocol; mysqldump (shipped as the
		// mariadb-client package in our image) handles both.
		return mysql.New(cfg, logger.WithName(dbType)), nil
	case TypeMongo:
		return mongo.New(cfg, logger.WithName("mongo")), nil
	case TypeRedis:
		return redis.New(cfg, logger.WithName("redis")), nil
	default:
		return nil, fmt.Errorf("unsupported db-type %q", dbType)
	}
}
