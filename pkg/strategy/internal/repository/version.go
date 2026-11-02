package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	"google.golang.org/protobuf/types/known/structpb"
)

const VersionTable = `
CREATE TABLE IF NOT EXISTS strategy_version (
	id text PRIMARY KEY DEFAULT uuid_generate_v4(),
	strategy_id text NOT NULL REFERENCES strategy(id) ON DELETE CASCADE,
	rev int NOT NULL,
	service_image text NOT NULL REFERENCES service(image),
	parameters jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE (strategy_id, rev)
);
`

type Version struct {
	Conn postgres.Query
}

func (db *Version) Create(ctx context.Context, strategyName, serviceImage string, parameters *structpb.Struct) (*pb.Version, error) {
	params, err := parametersToJSON(parameters)
	if err != nil {
		return nil, err
	}

	var id string
	err = db.Conn.QueryRow(ctx,
		`INSERT INTO strategy_version (strategy_id, rev, service_image, parameters)
		VALUES (
			(SELECT id FROM strategy WHERE name=$1),
			(SELECT COALESCE(MAX(rev), 0) + 1 FROM strategy_version v
				JOIN strategy s ON v.strategy_id = s.id WHERE s.name=$1),
			$2, $3
		) RETURNING id`,
		strategyName, serviceImage, params,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy version: %w", err)
	}

	return db.Get(ctx, id)
}

func (db *Version) Get(ctx context.Context, id string) (*pb.Version, error) {
	v, err := db.scanOne(ctx, versionSelect+` WHERE v.id=$1`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy version: %w", err)
	}

	return v, nil
}

func (db *Version) GetByStrategyIDAndRev(ctx context.Context, strategyID string, rev int32) (*pb.Version, error) {
	v, err := db.scanOne(ctx, versionSelect+` WHERE v.strategy_id=$1 AND v.rev=$2`, strategyID, rev)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy version: %w", err)
	}
	return v, nil
}

func (db *Version) GetByStrategyAndRev(ctx context.Context, strategyName string, rev int32) (*pb.Version, error) {
	v, err := db.scanOne(ctx, versionSelect+` WHERE s.name=$1 AND v.rev=$2`, strategyName, rev)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy version: %w", err)
	}

	return v, nil
}

func (db *Version) ListByStrategy(ctx context.Context, strategyName string) ([]*pb.Version, error) {
	rows, err := db.Conn.Query(ctx, versionSelect+` WHERE s.name=$1 ORDER BY v.rev ASC`, strategyName)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy versions: %w", err)
	}
	defer rows.Close()

	return parseVersionRows(rows)
}

const versionSelect = `
SELECT v.id, v.strategy_id, s.name, v.rev, v.service_image, v.parameters, v.created_at
FROM strategy_version v
JOIN strategy s ON v.strategy_id = s.id`

func (db *Version) scanOne(ctx context.Context, query string, args ...interface{}) (*pb.Version, error) {
	v := pb.Version{}
	var params []byte
	var createdAt time.Time

	err := db.Conn.QueryRow(ctx, query, args...).Scan(
		&v.Id, &v.StrategyId, &v.StrategyName, &v.Rev, &v.ServiceImage, &params, &createdAt,
	)
	if err != nil {
		return nil, err
	}

	v.Parameters, err = parametersFromJSON(params)
	if err != nil {
		return nil, err
	}

	v.CreatedAt = internal_pb.TimeToProtoTimestamp(createdAt)

	return &v, nil
}

func parseVersionRows(rows pgx.Rows) ([]*pb.Version, error) {
	versions := []*pb.Version{}

	for rows.Next() {
		v := pb.Version{}
		var params []byte
		var createdAt time.Time

		err := rows.Scan(
			&v.Id, &v.StrategyId, &v.StrategyName, &v.Rev, &v.ServiceImage, &params, &createdAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan strategy version: %w", err)
		}

		v.Parameters, err = parametersFromJSON(params)
		if err != nil {
			return nil, err
		}

		v.CreatedAt = internal_pb.TimeToProtoTimestamp(createdAt)
		versions = append(versions, &v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read strategy versions: %w", err)
	}

	return versions, nil
}

func parametersToJSON(s *structpb.Struct) ([]byte, error) {
	if s == nil {
		return nil, nil
	}

	b, err := s.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to encode parameters: %w", err)
	}

	return b, nil
}

func parametersFromJSON(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	s := &structpb.Struct{}
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("failed to decode parameters: %w", err)
	}

	return s, nil
}
