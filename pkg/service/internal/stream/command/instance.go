package command

import (
	"context"
	"fmt"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	st "github.com/lhjnilsson/foreverbull/pkg/service/stream"
)

// InstanceInterview, InstanceSanityCheck, and InstanceStop have no
// implementation. Each answers Unimplemented so a caller never reads it as
// done.
func InstanceInterview(ctx context.Context, message stream.Message) error {
	var instance st.InstanceInterviewCommand

	err := message.ParsePayload(&instance)
	if err != nil {
		return fmt.Errorf("error unmarshalling InstanceInterview payload: %w", err)
	}
	/*
		services := repository.Service{Conn: db}
		instances := repository.Instance{Conn: db}

		i, err := instances.Get(ctx, instance.ID)
		if err != nil {
			return fmt.Errorf("error getting instance: %w", err)
		}

		algorithm, err := i.GetInfo()
		if err != nil {
			return fmt.Errorf("error reading instance info: %w", err)
		}
		if algorithm != nil {
			err = services.SetAlgorithm(ctx, *i.Image, algorithm)
			if err != nil {
				return fmt.Errorf("error setting algorithm: %w", err)
			}
		}
	*/
	return unimplemented(usercopy.InterviewUnimplemented)
}

func InstanceSanityCheck(ctx context.Context, message stream.Message) error {
	/*
		db := message.MustGet(stream.DBDep).(postgres.Query)

		var instance st.InstanceSanityCheckCommand
		err := message.ParsePayload(&instance)
		if err != nil {
			return fmt.Errorf("error unmarshalling InstanceSanityCheck payload: %w", err)
		}

		instances := repository.Instance{Conn: db}
		checkOnline := func(ctx context.Context, id string) error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					i, err := instances.Get(ctx, id)
					if err != nil {
						var pgError *pgconn.PgError
						if errors.As(err, &pgError); pgError.Code == "02000" {
							time.Sleep(time.Second / 5)
							continue
						}
						return fmt.Errorf("error getting instance: %w", err)
					}
					if i.Statuses[0].Status == entity.InstanceStatusStopped {
						return fmt.Errorf("instance is stopped")
					}
					if i.Statuses[0].Status == entity.InstanceStatusError {
						return fmt.Errorf("instance is in error state: %s", *i.Statuses[0].Error)
					}

					if i.Host == nil || i.Port == nil {
						time.Sleep(time.Second / 5)
						continue
					}
					_, err = i.GetInfo()
					if err != nil {
						return fmt.Errorf("error reading instance info: %w", err)
					}
					return nil
				}
			}
		}
		ctx, cancel := context.WithTimeout(ctx, time.Second*30)
		defer cancel()
		g, gctx := errgroup.WithContext(ctx)
		for _, id := range instance.IDs {
			id := id
			g.Go(func() error {
				return checkOnline(gctx, id)
			})
		}
		err = g.Wait()
		if err != nil {
			return fmt.Errorf("error while checking for instances to come online: %w", err)
		}
	*/
	return unimplemented(usercopy.SanityCheckUnimplemented)
}

func InstanceStop(ctx context.Context, message stream.Message) error {
	/*
		instance := st.InstanceStopCommand{}
		err := message.ParsePayload(&instance)
		if err != nil {
			return fmt.Errorf("error unmarshalling InstanceStop payload: %w", err)
		}

		container := message.MustGet(dependency.ContainerDep).(container.Container)
		err = container.Stop(ctx, instance.ID, true)
		if err != nil {
			return err
		}
	*/
	return unimplemented(usercopy.InstanceStopUnimplemented)
}
