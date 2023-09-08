/*
 *  Copyright (c) 2023 Juice Technologies, Inc. All Rights Reserved.
 */
package app

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Juice-Labs/Juice-Labs/pkg/errors"
	"github.com/Juice-Labs/Juice-Labs/pkg/gpu"
	"github.com/Juice-Labs/Juice-Labs/pkg/logger"
	"github.com/Juice-Labs/Juice-Labs/pkg/restapi"
	"github.com/Juice-Labs/Juice-Labs/pkg/task"
	"github.com/Juice-Labs/Juice-Labs/pkg/utilities"
)

var (
	ErrClosed = errors.New("session is closed")
)

type Session struct {
	Id         string
	Version    string
	Persistent bool

	juicePath string
	gpus      *gpu.SelectedGpuSet

	closed             *utilities.ConcurrentVariable[bool]
	connections        *utilities.ConcurrentMap[string, *Connection]
	connectionsChanged chan struct{}
	sessionClosing     chan struct{}

	taskManager *task.TaskManager

	eventListener EventListener
}

func newSession(ctx context.Context, id string, version string, persistent bool, juicePath string, gpus *gpu.SelectedGpuSet, eventListener EventListener) *Session {
	return &Session{
		Id:                 id,
		Version:            version,
		Persistent:         persistent,
		juicePath:          juicePath,
		gpus:               gpus,
		closed:             utilities.NewConcurrentVariableD[bool](false),
		connections:        utilities.NewConcurrentMap[string, *Connection](),
		connectionsChanged: make(chan struct{}),
		sessionClosing:     make(chan struct{}),
		taskManager:        task.NewTaskManager(ctx),
		eventListener:      eventListener,
	}
}

func (session *Session) Session() restapi.Session {
	return utilities.WithReturn(session.closed, func(value bool) restapi.Session {
		connections := make([]restapi.Connection, 0, session.connections.Len())
		gpus := make([]restapi.SessionGpu, 0)
		state := restapi.SessionClosed

		if !value {
			session.connections.Foreach(func(key string, value *Connection) bool {
				connections = append(connections, restapi.Connection{
					ConnectionData: value.ConnectionData,
				})
				return true
			})

			gpus = session.gpus.GetGpus()
			state = restapi.SessionActive
		}

		return restapi.Session{
			Id:          session.Id,
			State:       state,
			Version:     session.Version,
			Gpus:        gpus,
			Connections: connections,
			Persistent:  session.Persistent,
		}
	})
}

func (session *Session) Run(group task.Group) error {
	group.GoFn(fmt.Sprintf("session %s close", session.Id), func(g task.Group) error {
		select {
		case <-group.Ctx().Done():
			session.Cancel()
			break

		case <-session.taskManager.Ctx().Done():
			break
		}

		session.closed.Set(true)

		err := session.taskManager.Wait()

		close(session.connectionsChanged)
		close(session.sessionClosing)

		session.gpus.Release()

		session.eventListener.SessionClosed(session.Id)

		return err
	})

	if !session.Persistent {
		group.GoFn(fmt.Sprintf("session %s ticker", session.Id), func(g task.Group) error {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			done := false
			for !done {
				select {
				case <-session.sessionClosing:
					done = true

				case <-ticker.C:
					session.Cancel()

				case <-session.connectionsChanged:
					if session.connections.Empty() {
						ticker.Reset(30 * time.Second)
					} else {
						ticker.Stop()
					}
				}
			}

			return nil
		})
	}

	return nil
}

func (session *Session) Cancel() {
	utilities.With(session.closed, func(value bool) {
		if !value {
			session.taskManager.Cancel()
		}
	})
}

func (session *Session) Connect(connectionData restapi.ConnectionData, c net.Conn) error {
	logger.Tracef("Connecting to connection: %s", connectionData.Id)

	return utilities.WithReturn(session.closed, func(value bool) error {
		if !value {
			connection, found := session.connections.Get(connectionData.Id)
			if !found {
				var err error
				connection, err = session.addConnection(connectionData)
				if err != nil {
					return errors.Newf("session %s connection %s failed to connect", session.Id, connectionData.Id).Wrap(err)
				}
			}

			return connection.Connect(c)
		}

		return ErrClosed
	})
}

func (session *Session) addConnection(connectionData restapi.ConnectionData) (*Connection, error) {
	logger.Tracef("session %s creating connection %s for PID %s and process name %s", session.Id, connectionData.Id, connectionData.Pid, connectionData.ProcessName)

	exitCodeCh := make(chan int)

	connection := newConnection(connectionData, session.juicePath, session.gpus.GetPciBusString())
	err := connection.Start(session.taskManager, exitCodeCh)
	if err != nil {
		return nil, err
	}

	session.taskManager.GoFn(fmt.Sprintf("session %s connection %s", session.Id, connection.Id), func(g task.Group) error {
		exitCode, ok := <-exitCodeCh
		if !ok {
			panic("channel has been closed")
		}
		close(exitCodeCh)

		session.connections.Delete(connection.Id)
		session.connectionsChanged <- struct{}{}

		session.eventListener.ConnectionClosed(session.Id, connection.ConnectionData, exitCode)

		return nil
	})

	session.connections.Set(connection.Id, connection)
	session.connectionsChanged <- struct{}{}

	session.eventListener.ConnectionCreated(session.Id, connection.ConnectionData)

	return connection, nil
}
