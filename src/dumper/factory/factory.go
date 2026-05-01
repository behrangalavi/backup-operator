package factory

import (
	"fmt"

	"backup-operator/dumper"
	"backup-operator/dumper/mongo"
	"backup-operator/dumper/mysql"
	"backup-operator/dumper/postgres"

	"github.com/go-logr/logr"
)

const (
	TypePostgres = "postgres"
	TypeMySQL    = "mysql"
	TypeMongo    = "mongo"
)

// NewDumper creates the right Dumper for the given db-type label.
// Add new database types by implementing dumper.Dumper and registering here.
// Never branch on db type outside this factory.
func NewDumper(dbType string, cfg dumper.Config, logger logr.Logger) (dumper.Dumper, error) {
	switch dbType {
	case TypePostgres:
		return postgres.New(cfg, logger.WithName("postgres")), nil
	case TypeMySQL:
		return mysql.New(cfg, logger.WithName("mysql")), nil
	case TypeMongo:
		return mongo.New(cfg, logger.WithName("mongo")), nil
	default:
		return nil, fmt.Errorf("unsupported db-type %q", dbType)
	}
}
