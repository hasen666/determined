package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/pkg/actor"
	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/searcher"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/experimentv1"
)

func (a *apiServer) GetExperiments(
	_ context.Context, req *apiv1.GetExperimentsRequest) (*apiv1.GetExperimentsResponse, error) {
	resp := &apiv1.GetExperimentsResponse{}
	if err := a.m.db.QueryProto("get_experiments", &resp.Experiments); err != nil {
		return nil, err
	}
	a.filter(&resp.Experiments, func(i int) bool {
		v := resp.Experiments[i]
		if req.Archived != nil && req.Archived.Value != v.Archived {
			return false
		}
		found := false
		for _, state := range req.States {
			if state == v.State {
				found = true
				break
			}
		}
		if len(req.States) != 0 && !found {
			return false
		}
		found = false
		for _, user := range req.Users {
			if user == v.Username {
				found = true
				break
			}
		}
		if len(req.Users) != 0 && !found {
			return false
		}
		return strings.Contains(strings.ToLower(v.Description), strings.ToLower(req.Description))
	})
	a.sort(resp.Experiments, req.OrderBy, req.SortBy, apiv1.GetExperimentsRequest_SORT_BY_ID)
	return resp, a.paginate(&resp.Pagination, &resp.Experiments, req.Offset, req.Limit)
}

func (a *apiServer) PreviewHPSearch(
	_ context.Context, req *apiv1.PreviewHPSearchRequest) (*apiv1.PreviewHPSearchResponse, error) {
	bytes, err := protojson.Marshal(req.Config)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "error parsing experiment config: %s", err)
	}
	config := model.DefaultExperimentConfig()
	if err = json.Unmarshal(bytes, &config); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "error parsing experiment config: %s", err)
	}
	if err = check.Validate(config.Searcher); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid experiment config: %s", err)
	}

	sm := searcher.NewSearchMethod(config.Searcher)
	s := searcher.NewSearcher(req.Seed, sm, config.Hyperparameters)
	sim, err := searcher.Simulate(s, nil, searcher.RandomValidation, true, config.Searcher.Metric)
	if err != nil {
		return nil, err
	}
	protoSim := &experimentv1.ExperimentSimulation{Seed: req.Seed}
	indexes := make(map[string]int)
	toProto := func(op searcher.Runnable) (experimentv1.RunnableOperation, error) {
		switch op := op.(type) {
		case searcher.Train:
			switch op.Length.Unit {
			case model.Records:
				return experimentv1.RunnableOperation{
					Type: experimentv1.RunnableType_RUNNABLE_TYPE_TRAIN,
					Length: &experimentv1.TrainingUnits{
						Unit:  experimentv1.Unit_UNIT_RECORDS,
						Count: int32(op.Length.Units),
					},
				}, nil
			case model.Batches:
				return experimentv1.RunnableOperation{
					Type: experimentv1.RunnableType_RUNNABLE_TYPE_TRAIN,
					Length: &experimentv1.TrainingUnits{
						Unit:  experimentv1.Unit_UNIT_BATCHES,
						Count: int32(op.Length.Units),
					},
				}, nil
			case model.Epochs:
				return experimentv1.RunnableOperation{
					Type: experimentv1.RunnableType_RUNNABLE_TYPE_TRAIN,
					Length: &experimentv1.TrainingUnits{
						Unit:  experimentv1.Unit_UNIT_EPOCHS,
						Count: int32(op.Length.Units),
					},
				}, nil
			default:
				return experimentv1.RunnableOperation{},
					fmt.Errorf("unrecognized unit %s", op.Length.Unit)
			}
		case searcher.Validate:
			return experimentv1.RunnableOperation{
				Type: experimentv1.RunnableType_RUNNABLE_TYPE_VALIDATE,
			}, nil
		case searcher.Checkpoint:
			return experimentv1.RunnableOperation{
				Type: experimentv1.RunnableType_RUNNABLE_TYPE_CHECKPOINT,
			}, nil
		default:
			return experimentv1.RunnableOperation{},
				fmt.Errorf("unrecognized searcher.Runnable %s", op)
		}
	}
	for _, result := range sim.Results {
		var operations []*experimentv1.RunnableOperation
		for _, msg := range result {
			op, err := toProto(msg)
			if err != nil {
				return nil, errors.Wrapf(err, "error converting msg in simultion result %s", msg)
			}
			operations = append(operations, &op)
		}
		hash := fmt.Sprint(operations)
		if i, ok := indexes[hash]; ok {
			protoSim.Trials[i].Occurrences++
		} else {
			protoSim.Trials = append(protoSim.Trials,
				&experimentv1.TrialSimulation{Operations: operations, Occurrences: 1})
			indexes[hash] = len(protoSim.Trials) - 1
		}
	}
	return &apiv1.PreviewHPSearchResponse{Simulation: protoSim}, nil
}

func (a *apiServer) ActivateExperiment(
	ctx context.Context, req *apiv1.ActivateExperimentRequest,
) (resp *apiv1.ActivateExperimentResponse, err error) {
	ok, err := a.m.db.CheckExperimentExists(int(req.Id))
	switch {
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed to check if experiment exists: %s", err)
	case !ok:
		return nil, status.Errorf(codes.NotFound, "experiment %d not found", req.Id)
	}

	addr := actor.Addr("experiments", req.Id).String()
	switch err = a.actorRequest(addr, req, &resp); {
	case status.Code(err) == codes.NotFound:
		return nil, status.Error(codes.FailedPrecondition, "experiment in terminal state")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed passing request to experiment actor: %s", err)
	default:
		return resp, nil
	}
}

func (a *apiServer) PauseExperiment(
	ctx context.Context, req *apiv1.PauseExperimentRequest,
) (resp *apiv1.PauseExperimentResponse, err error) {
	ok, err := a.m.db.CheckExperimentExists(int(req.Id))
	switch {
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	case !ok:
		return nil, status.Errorf(codes.NotFound, "experiment %d not found", req.Id)
	}

	addr := actor.Addr("experiments", req.Id).String()
	switch err = a.actorRequest(addr, req, &resp); {
	case status.Code(err) == codes.NotFound:
		return nil, status.Error(codes.FailedPrecondition, "experiment in terminal state")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed passing request to experiment actor: %s", err)
	default:
		return resp, nil
	}
}

func (a *apiServer) CancelExperiment(
	ctx context.Context, req *apiv1.CancelExperimentRequest,
) (resp *apiv1.CancelExperimentResponse, err error) {
	ok, err := a.m.db.CheckExperimentExists(int(req.Id))
	switch {
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed to check if experiment exists: %s", err)
	case !ok:
		return nil, status.Errorf(codes.NotFound, "experiment %d not found", req.Id)
	}

	addr := actor.Addr("experiments", req.Id).String()
	err = a.actorRequest(addr, req, &resp)
	if status.Code(err) == codes.NotFound {
		return &apiv1.CancelExperimentResponse{}, nil
	}
	return resp, err
}

func (a *apiServer) KillExperiment(
	ctx context.Context, req *apiv1.KillExperimentRequest,
) (
	resp *apiv1.KillExperimentResponse, err error) {
	ok, err := a.m.db.CheckExperimentExists(int(req.Id))
	switch {
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed to check if experiment exists: %s", err)
	case !ok:
		return nil, status.Errorf(codes.NotFound, "experiment %d not found", req.Id)
	}

	addr := actor.Addr("experiments", req.Id).String()
	err = a.actorRequest(addr, req, &resp)
	if status.Code(err) == codes.NotFound {
		return &apiv1.KillExperimentResponse{}, nil
	}
	return resp, err
}
