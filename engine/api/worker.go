package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-gorp/gorp"
	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/authentication"
	workerauth "github.com/ovh/cds/engine/api/authentication/worker"
	"github.com/ovh/cds/engine/api/services"
	"github.com/ovh/cds/engine/api/worker"
	"github.com/ovh/cds/engine/api/workflow"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

func (api *API) postRegisterWorkerHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		// First get the jwt token to checks where this registration is coming from
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if jwt == "" {
			return sdk.WithStack(sdk.ErrUnauthorized)
		}

		var registrationForm sdk.WorkerRegistrationForm
		if err := service.UnmarshalBody(r, &registrationForm); err != nil {
			return err
		}

		// Check that the worker can authentify on CDS API
		workerTokenFromHatchery, err := workerauth.VerifyToken(ctx, api.mustDB(), jwt)
		if err != nil {
			return sdk.NewErrorWithStack(sdk.WrapError(err, "unauthorized worker jwt token %s", jwt), sdk.ErrUnauthorized)
		}

		// Check that hatchery exists
		hatchSrv, err := services.LoadByNameAndType(ctx, api.mustDB(), workerTokenFromHatchery.Worker.HatcheryName, sdk.TypeHatchery)
		if err != nil {
			return sdk.WrapError(err, "unable to load hatchery %s", workerTokenFromHatchery.Worker.HatcheryName)
		}

		// Retrieve the authentifed Consumer from the hatchery
		hatcheryConsumer, err := authentication.LoadConsumerByID(ctx, api.mustDB(), *hatchSrv.ConsumerID, authentication.LoadConsumerOptions.WithAuthentifiedUser)
		if err != nil {
			return sdk.WrapError(err, "unable to load consumer %v", hatchSrv.ConsumerID)
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		defer tx.Rollback() // nolint

		var groupIDs []int64
		if workerTokenFromHatchery.Worker.JobID != 0 {
			job, err := workflow.LoadNodeJobRun(ctx, tx, api.Cache, workerTokenFromHatchery.Worker.JobID)
			if err != nil {
				return sdk.NewErrorWithStack(sdk.WrapError(err, "error on LoadNodeJobRun with jobID %d", workerTokenFromHatchery.Worker.JobID), sdk.ErrForbidden)
			}
			groupIDs = sdk.Groups(job.ExecGroups).ToIDs()
		} else {
			groupIDs = hatcheryConsumer.GetGroupIDs()
		}

		// We have to issue a new consumer for the worker
		workerConsumer, err := authentication.NewConsumerWorker(ctx, tx, workerTokenFromHatchery.Subject, hatchSrv, hatcheryConsumer, groupIDs)
		if err != nil {
			return err
		}

		// Try to register worker
		wk, err := worker.RegisterWorker(ctx, tx, api.Cache, workerTokenFromHatchery.Worker, *hatchSrv, workerConsumer, registrationForm)
		if err != nil {
			return sdk.NewErrorWithStack(
				sdk.WrapError(err, "[%s] Registering failed", workerTokenFromHatchery.Worker.WorkerName),
				sdk.ErrUnauthorized,
			)
		}

		log.Debug("New worker: [%s] - %s", wk.ID, wk.Name)

		workerSession, err := authentication.NewSession(ctx, tx, workerConsumer, workerauth.SessionDuration, false)
		if err != nil {
			return sdk.NewErrorWithStack(
				sdk.WrapError(err, "[%s] Registering failed", workerTokenFromHatchery.Worker.WorkerName),
				sdk.ErrUnauthorized,
			)
		}

		if err := tx.Commit(); err != nil {
			return sdk.WithStack(err)
		}

		jwt, err = authentication.NewSessionJWT(workerSession)
		if err != nil {
			return sdk.NewErrorWithStack(
				sdk.WrapError(err, "[%s] Registering failed", workerTokenFromHatchery.Worker.WorkerName),
				sdk.ErrUnauthorized,
			)
		}

		// Set the JWT token as a header
		log.Debug("worker.registerWorkerHandler> X-CDS-JWT:%s", jwt[:12])
		w.Header().Add("X-CDS-JWT", jwt)

		// Return worker info to worker itself
		return service.WriteJSON(w, wk, http.StatusOK)
	}
}

func (api *API) getWorkerHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		name := vars["name"]

		withKey := FormBool(r, "withKey")

		if !isCDN(ctx) {
			return sdk.WrapError(sdk.ErrForbidden, "only CDN can call this route")
		}

		var wkr *sdk.Worker
		var err error
		if withKey {
			wkr, err = worker.LoadWorkerByNameWithDecryptKey(ctx, api.mustDB(), name)
			if wkr != nil {
				encoded := base64.StdEncoding.EncodeToString(wkr.PrivateKey)
				wkr.PrivateKey = []byte(encoded)
			}
		} else {
			wkr, err = worker.LoadWorkerByName(ctx, api.mustDB(), name)
		}
		if err != nil {
			return err
		}
		return service.WriteJSON(w, wkr, http.StatusOK)
	}
}

func (api *API) getWorkersHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		var workers []sdk.Worker
		var err error
		if isHatchery(ctx) {
			workers, err = worker.LoadAllByHatcheryID(ctx, api.mustDB(), getAPIConsumer(ctx).Service.ID)
			if err != nil {
				return err
			}
		} else if isMaintainer(ctx) {
			workers, err = worker.LoadAll(ctx, api.mustDB())
			if err != nil {
				return err
			}
		}
		// TODO load workers for users
		return service.WriteJSON(w, workers, http.StatusOK)
	}
}

func (api *API) disableWorkerHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		// Get pipeline and action name in URL
		vars := mux.Vars(r)
		id := vars["id"]

		wk, err := worker.LoadByID(ctx, api.mustDB(), id)
		if err != nil {
			return err
		}

		if !isAdmin(ctx) {
			if wk.Status == sdk.StatusBuilding {
				return sdk.WrapError(sdk.ErrForbidden, "Cannot disable a worker with status %s", wk.Status)
			}
			hatcherySrv, err := services.LoadByConsumerID(ctx, api.mustDB(), getAPIConsumer(ctx).ID)
			if err != nil {
				return sdk.WrapError(sdk.ErrForbidden, "Cannot disable a worker from this hatchery: %v", err)
			}
			if wk.HatcheryID == nil {
				return sdk.WrapError(sdk.ErrForbidden, "hatchery %d cannot disable worker %s started by %s that is no more linked to an hatchery", hatcherySrv.ID, wk.ID, wk.HatcheryName)
			}
			if *wk.HatcheryID != hatcherySrv.ID {
				return sdk.WrapError(sdk.ErrForbidden, "cannot disable a worker from hatchery (expected: %d/actual: %d)", *wk.HatcheryID, hatcherySrv.ID)
			}
		}

		if err := DisableWorker(ctx, api.mustDB(), id, api.Config.Log.StepMaxSize); err != nil {
			cause := sdk.Cause(err)
			if cause == worker.ErrNoWorker || cause == sql.ErrNoRows {
				return sdk.WrapError(sdk.ErrWrongRequest, "disableWorkerHandler> worker %s does not exists", id)
			}
			return sdk.WrapError(err, "cannot update worker status")
		}

		return nil
	}
}

func (api *API) postRefreshWorkerHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		wk, err := worker.LoadByConsumerID(ctx, api.mustDB(), getAPIConsumer(ctx).ID)
		if err != nil {
			return err
		}

		if err := worker.RefreshWorker(api.mustDB(), wk.ID); err != nil && (sdk.Cause(err) != sql.ErrNoRows || sdk.Cause(err) != worker.ErrNoWorker) {
			return sdk.WrapError(err, "cannot refresh last beat of %s", wk.Name)
		}
		return nil
	}
}

func (api *API) postUnregisterWorkerHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		wk, err := worker.LoadByConsumerID(ctx, api.mustDB(), getAPIConsumer(ctx).ID)
		if err != nil {
			return err
		}
		if err := DisableWorker(ctx, api.mustDB(), wk.ID, api.Config.Log.StepMaxSize); err != nil {
			return sdk.WrapError(err, "cannot delete worker %s", wk.Name)
		}
		return nil
	}
}

func (api *API) workerWaitingHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		wk, err := worker.LoadByConsumerID(ctx, api.mustDB(), getAPIConsumer(ctx).ID)
		if err != nil {
			return err
		}

		if wk.Status == sdk.StatusWaiting {
			return nil
		}

		if wk.Status != sdk.StatusChecking && wk.Status != sdk.StatusBuilding {
			log.Debug("workerWaitingHandler> Worker %s cannot be Waiting. Current status: %s", wk.Name, wk.Status)
			return nil
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		defer tx.Rollback() // nolint

		if err := worker.SetStatus(ctx, tx, wk.ID, sdk.StatusWaiting); err != nil {
			return sdk.WrapError(err, "cannot update worker %s", wk.ID)
		}

		return sdk.WithStack(tx.Commit())
	}
}

// After migration to new CDS Workflow, put DisableWorker into
// the package workflow

// DisableWorker disable a worker
func DisableWorker(ctx context.Context, db *gorp.DbMap, id string, maxLogSize int64) error {
	tx, errb := db.Begin()
	if errb != nil {
		return fmt.Errorf("DisableWorker> Cannot start tx: %v", errb)
	}
	defer tx.Rollback() // nolint

	query := `SELECT name, status, job_run_id FROM worker WHERE id = $1 FOR UPDATE`
	var st, name string
	var jobID sql.NullInt64
	if err := tx.QueryRow(query, id).Scan(&name, &st, &jobID); err != nil {
		log.Debug("DisableWorker[%s]> Cannot lock worker: %v", id, err)
		return nil
	}

	if st == sdk.StatusBuilding && jobID.Valid {
		// Worker is awol while building !
		// We need to restart this action
		wNodeJob, errL := workflow.LoadNodeJobRun(ctx, tx, nil, jobID.Int64)
		if errL == nil && wNodeJob.Retry < 3 {
			if err := workflow.RestartWorkflowNodeJob(context.TODO(), db, *wNodeJob, maxLogSize); err != nil {
				log.Warning(ctx, "DisableWorker[%s]> Cannot restart workflow node run: %v", name, err)
			} else {
				log.Info(ctx, "DisableWorker[%s]> WorkflowNodeRun %d restarted after crash", name, jobID.Int64)
			}
		}

		log.Info(ctx, "DisableWorker> Worker %s crashed while building %d !", name, jobID.Int64)
	}

	if err := worker.SetStatus(ctx, tx, id, sdk.StatusDisabled); err != nil {
		cause := sdk.Cause(err)
		if cause == worker.ErrNoWorker || cause == sql.ErrNoRows {
			return sdk.WrapError(sdk.ErrWrongRequest, "DisableWorker> worker %s does not exists", id)
		}
		return sdk.WrapError(err, "cannot update worker status")
	}

	return tx.Commit()
}
