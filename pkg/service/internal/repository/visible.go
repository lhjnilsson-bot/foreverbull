package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/service"
)

// ListVisible returns services the caller may see. It joins status by service id.
func (db *Service) ListVisible(ctx context.Context, caller auth.Account) ([]*pb.Service, error) {
	if caller.ID == "" {
		return nil, errors.New("account id is empty")
	}
	where, args := serviceVisibleWhere(caller)
	rows, err := db.Conn.Query(ctx, `
		SELECT service.id
		FROM service
		WHERE `+where+`
		ORDER BY service.image`, args...)
	if err != nil {
		return nil, fmt.Errorf("list visible services: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list visible services: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list visible services: %w", err)
	}
	out := make([]*pb.Service, 0, len(ids))
	for _, id := range ids {
		item, err := db.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func serviceVisibleWhere(caller auth.Account) (string, []any) {
	switch caller.Role {
	case auth.RoleAdmin:
		return "TRUE", nil
	case auth.RoleEditor:
		return `(service.owner_id = $1 OR EXISTS (
			SELECT 1 FROM domain_grant AS gr
			WHERE gr.domain_kind = $2 AND gr.domain_id = service.id AND gr.grantee_id = $1))`,
			[]any{caller.ID, auth.DomainService}
	default:
		return `EXISTS (
			SELECT 1 FROM domain_grant AS gr
			WHERE gr.domain_kind = $2 AND gr.domain_id = service.id AND gr.grantee_id = $1)`,
			[]any{caller.ID, auth.DomainService}
	}
}

// GetByID loads one service and its status history after the caller was authorized.
func (db *Service) GetByID(ctx context.Context, id string) (*pb.Service, error) {
	if id == "" {
		return nil, ErrServiceNotFound
	}
	rows, err := db.Conn.Query(ctx, `
		SELECT service.image, algorithm, ss.status, ss.error, ss.occurred_at,
		       COALESCE(p.username, g.github_login, '')
		FROM service
		INNER JOIN service_status AS ss ON ss.service_id = service.id
		LEFT JOIN account_password AS p ON p.account_id = service.owner_id
		LEFT JOIN account_github AS g ON g.account_id = service.owner_id
		WHERE service.id = $1
		ORDER BY ss.occurred_at ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}
	defer rows.Close()
	return scanServiceRows(rows)
}

// CreateOwned inserts a service for ownerID. The status trigger records service id.
func (db *Service) CreateOwned(ctx context.Context, ownerID, image string) (*pb.Service, error) {
	if ownerID == "" {
		return nil, errors.New("account id is empty")
	}
	var id string
	err := db.Conn.QueryRow(ctx, `
		INSERT INTO service (image, owner_id) VALUES ($1, $2) RETURNING id`, image, ownerID).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, auth.ErrAlreadyExists
		}
		return nil, fmt.Errorf("failed to create service: %w", err)
	}
	return db.GetByID(ctx, id)
}

func (db *Service) getByImageJoined(ctx context.Context, image string) (*pb.Service, error) {
	rows, err := db.Conn.Query(ctx, `
		SELECT service.id
		FROM service
		WHERE service.image = $1
		ORDER BY service.id
		LIMIT 1`, image)
	if err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}
	defer rows.Close()
	var id string
	if rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to get service: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}
	if id == "" {
		return nil, ErrServiceNotFound
	}
	return db.GetByID(ctx, id)
}

func scanServiceRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) (*pb.Service, error) {
	service := pb.Service{}
	algorithm := []byte{}
	for rows.Next() {
		status := pb.Service_Status{}
		occurred := time.Time{}
		err := rows.Scan(&service.Image, &algorithm, &status.Status, &status.Error, &occurred, &service.OwnerLogin)
		if err != nil {
			return nil, fmt.Errorf("failed to get service: %w", err)
		}
		status.OccurredAt = internal_pb.TimeToProtoTimestamp(occurred)
		service.Statuses = append(service.Statuses, &status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}
	if service.Image == "" {
		return nil, ErrServiceNotFound
	}
	if len(algorithm) == 0 {
		return &service, nil
	}
	if err := json.Unmarshal(algorithm, &service.Algorithm); err != nil {
		return nil, fmt.Errorf("failed to decode algorithm: %w", err)
	}
	return &service, nil
}
