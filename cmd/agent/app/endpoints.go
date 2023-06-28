/*
 *  Copyright (c) 2023 Juice Technologies, Inc. All Rights Reserved.
 */
package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/Juice-Labs/Juice-Labs/cmd/agent/prometheus"
	"github.com/Juice-Labs/Juice-Labs/internal/build"
	"github.com/Juice-Labs/Juice-Labs/pkg/api"
	"github.com/Juice-Labs/Juice-Labs/pkg/gpu"
	"github.com/Juice-Labs/Juice-Labs/pkg/logger"
	pkgnet "github.com/Juice-Labs/Juice-Labs/pkg/net"
	"github.com/Juice-Labs/Juice-Labs/pkg/utilities"
)

const (
	RequestSessionName = "RequestSession"
)

func (agent *Agent) initializeEndpoints() {
	agent.Server.AddCreateEndpoint(agent.getStatusEp)
	agent.Server.SetCreateEndpoint(RequestSessionName, agent.requestSessionEp)
	agent.Server.AddCreateEndpoint(agent.getSessionEp)
	agent.Server.AddCreateEndpoint(agent.connectSessionEp)

	prometheus.InitializeEndpoints(agent.Server)
}

func (agent *Agent) getStatusEp(router *mux.Router) error {
	router.Methods("GET").Path("/v1/status").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			err := pkgnet.Respond(w, http.StatusOK, api.Agent{
				Id:          agent.Id,
				State:       api.StateActive,
				Version:     build.Version,
				Hostname:    agent.Hostname,
				Address:     agent.Server.Address(),
				MaxSessions: agent.maxSessions,
				Gpus:        agent.Gpus.GetGpus(),
			})

			if err != nil {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
				logger.Error(err)
			}
		})
	return nil
}

func (agent *Agent) requestSessionEp(router *mux.Router) error {
	router.Methods("POST").Path("/v1/request/session").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var selectedGpus gpu.SelectedGpuSet

			requestSession, err := pkgnet.ReadRequestBody[api.RequestSession](r)
			if err == nil {
				// TODO: Verify version

				if agent.sessions.Len()+1 >= agent.maxSessions {
					err = errors.New("unable to add another session")
				}
			} else {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
			}

			if err != nil {
				logger.Error(err)
				selectedGpus.Release()
				return
			}

			createdSession, err := agent.startSession(requestSession)
			if err != nil {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
				logger.Error(err)
				return
			}

			err = pkgnet.RespondWithString(w, http.StatusOK, createdSession.Id)
			if err != nil {
				err = errors.Join(err, createdSession.Signal())

				logger.Error(err)
			}
		})
	return nil
}

func (agent *Agent) registerSessionEp(router *mux.Router) error {
	router.Methods("POST").Path("/v1/register/session").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			session, err := pkgnet.ReadRequestBody[api.Session](r)
			if err != nil {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
				logger.Error(err)
				return
			}

			// TODO: verify it came from the controller

			err = agent.registerSession(session)
			if err != nil {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
				logger.Error(err)
				return
			}

			pkgnet.RespondEmpty(w, http.StatusOK)

			err = pkgnet.PostWithBodyNoResponse[api.Agent](agent.httpClient, getUrlString(fmt.Sprint("/v1/agent/", agent.Id)), api.Agent{
				Id:       agent.Id,
				State:    api.StateActive,
				Gpus:     agent.Gpus.GetGpus(),
				Sessions: agent.getSessions(),
			})
			if err != nil {
				logger.Error(err)
				return
			}
		})
	return nil
}

func (agent *Agent) getSessionEp(router *mux.Router) error {
	router.Methods("GET").Path("/v1/session/{id}").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			id := mux.Vars(r)["id"]

			session, err := agent.getSession(id)
			if err != nil {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
				logger.Error(err)
				return
			}

			err = pkgnet.Respond(w, http.StatusOK, session)
			if err != nil {
				logger.Error(err)
			}
		})
	return nil
}

func (agent *Agent) connectSessionEp(router *mux.Router) error {
	router.Methods("POST").Path("/v1/connect/session/{id}").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			id := mux.Vars(r)["id"]

			session, err := agent.getSession(id)
			if err == nil {
				if session == nil {
					pkgnet.RespondEmpty(w, http.StatusBadRequest)
				}
			} else {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
			}

			if err != nil {
				logger.Error(err)
				return
			}

			var conn net.Conn

			hijacker, err := utilities.Cast[http.Hijacker](w)
			if err == nil {
				conn, _, err = hijacker.Hijack()
				if err != nil {
					err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
				}
			} else {
				err = errors.Join(err, pkgnet.RespondWithString(w, http.StatusInternalServerError, err.Error()))
			}

			if err != nil {
				logger.Error(err)
				return
			}

			err = session.Connect(conn)
			if err != nil {
				err = errors.Join(err, conn.Close())

				logger.Error(err)
			}
		})
	return nil
}
