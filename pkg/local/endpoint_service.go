// Copyright (c) 2019 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package local

import (
	"context"

	"github.com/networkservicemesh/cmd-nsmgr/pkg/properties"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	mechanismCommon "github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/common"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"

	"github.com/networkservicemesh/sdk/pkg/tools/spanhelper"

	"github.com/golang/protobuf/ptypes/empty"

	"github.com/networkservicemesh/api/pkg/api/networkservice"

	"github.com/networkservicemesh/api/pkg/api/registry"
	unifiednsm "github.com/networkservicemesh/cmd-nsmgr/pkg/api/nsm"
	"github.com/networkservicemesh/cmd-nsmgr/pkg/common"
	"github.com/networkservicemesh/cmd-nsmgr/pkg/model"
)

// ConnectionService makes basic Mechanism selection for the incoming connection
type endpointService struct {
	nseManager unifiednsm.NetworkServiceEndpointManager
	props      *properties.Properties
	model      model.Model
}

func (cce *endpointService) closeEndpoint(ctx context.Context, cc *model.ClientConnection) error {
	span := spanhelper.FromContext(ctx, "closeEndpoint")
	defer span.Finish()
	ctx = span.Context()
	logger := span.Logger()

	if cc.Endpoint == nil {
		logger.Infof("No need to close, since NSE is we know is dead at this point.")
		return nil
	}
	closeCtx, closeCancel := context.WithTimeout(ctx, cce.props.CloseTimeout)
	defer closeCancel()

	client, nseClientError := cce.nseManager.CreateNSEClient(closeCtx, cc.Endpoint)

	if client != nil {
		if ld := cc.Xcon.GetDestination(); ld != nil {
			return client.Close(ctx, ld)
		}
		err := client.Cleanup()
		span.LogError(err)
	} else {
		span.LogError(nseClientError)
	}
	return nseClientError
}

func (cce *endpointService) Request(ctx context.Context, request *networkservice.NetworkServiceRequest) (*networkservice.Connection, error) {
	logger := common.Log(ctx)
	clientConnection := common.ModelConnection(ctx)
	dp := common.Forwarder(ctx)
	endpoint := common.Endpoint(ctx)

	if clientConnection == nil {
		return nil, errors.Errorf("client connection need to be passed")
	}
	client, err := cce.nseManager.CreateNSEClient(ctx, endpoint)
	if err != nil {
		// 7.2.6.1
		return nil, errors.Errorf("NSM:(7.2.6.1) Failed to create NSE Client. %v", err)
	}
	defer func() {
		if cleanupErr := client.Cleanup(); cleanupErr != nil {
			logger.Errorf("NSM:(7.2.6.2) Error during Cleanup: %v", cleanupErr)
		}
	}()

	var message *networkservice.NetworkServiceRequest
	if cce.nseManager.IsLocalEndpoint(endpoint) {
		message = cce.createLocalNSERequest(endpoint, request.Connection, dp.LocalMechanisms, clientConnection)
	} else {
		message = cce.createRemoteNSMRequest(endpoint, request.Connection, common.RemoteMechanisms(ctx), clientConnection)
	}
	logger.Infof("NSM:(7.2.6.2) Requesting NSE with request %v", message)

	span := spanhelper.FromContext(ctx, "nse.request")
	ctx = span.Context()
	defer span.Finish()
	span.LogObject("nse.request", message)

	nseConn, e := client.Request(ctx, message)
	span.LogObject("nse.response", nseConn)
	if e != nil {
		e = errors.Errorf("NSM:(7.2.6.2.1) error requesting networkservice from %+v with message %#v error: %s", endpoint, message, e)
		span.LogError(e)
		return nil, e
	}
	// 7.2.6.2.2
	if err = cce.updateConnectionContext(ctx, request.GetConnection(), nseConn); err != nil {
		err = errors.Errorf("NSM:(7.2.6.2.2) failure Validating NSE Connection: %s", err)
		span.LogError(err)
		return nil, err
	}

	// 7.2.6.2.3 update connection parameters, add workspace if local nse
	cce.updateConnectionParameters(nseConn, endpoint)

	ctx = common.WithEndpointConnection(ctx, nseConn)

	return common.ProcessNext(ctx, request)
}

func (cce *endpointService) Close(ctx context.Context, connection *networkservice.Connection) (*empty.Empty, error) {
	clientConnection := common.ModelConnection(ctx)
	if clientConnection != nil {
		if err := cce.closeEndpoint(ctx, clientConnection); err != nil {
			return &empty.Empty{}, err
		}
	}

	return common.ProcessClose(ctx, connection)
}

func (cce *endpointService) createLocalNSERequest(endpoint *registry.NSERegistration, requestConn *networkservice.Connection, localMechanisms []*networkservice.Mechanism, clientConnection *model.ClientConnection) *networkservice.NetworkServiceRequest {
	if clientConnection.ConnectionState == model.ClientConnectionHealing && endpoint == clientConnection.Endpoint {
		if localDst := clientConnection.Xcon.GetLocalDestination(); localDst != nil {
			return &networkservice.NetworkServiceRequest{
				Connection: &networkservice.Connection{
					Id:             localDst.GetId(),
					NetworkService: localDst.NetworkService,
					Context:        localDst.GetContext(),
					Labels:         localDst.GetLabels(),
					Path:           common.Strings2Path(cce.model.GetNsm().GetName()),
				},
				MechanismPreferences: localMechanisms,
			}
		}
	}

	return &networkservice.NetworkServiceRequest{
		Connection: &networkservice.Connection{
			Id:             cce.model.ConnectionID(), // ID for NSE is managed by NSMgr
			NetworkService: endpoint.GetNetworkService().GetName(),
			Path:           common.Strings2Path(cce.model.GetNsm().GetName()),
			Context:        requestConn.GetContext(),
			Labels:         requestConn.GetLabels(),
		},
		MechanismPreferences: localMechanisms,
	}
}

func (cce *endpointService) createRemoteNSMRequest(endpoint *registry.NSERegistration,
	requestConn *networkservice.Connection, remoteMechanisms []*networkservice.Mechanism, clientConnection *model.ClientConnection) *networkservice.NetworkServiceRequest {
	// Try Heal only if endpoint are same as for existing connection.
	if clientConnection.ConnectionState == model.ClientConnectionHealing && endpoint == clientConnection.Endpoint {
		if remoteDst := clientConnection.Xcon.GetRemoteDestination(); remoteDst != nil {
			return &networkservice.NetworkServiceRequest{
				Connection: &networkservice.Connection{
					Id:                         remoteDst.GetId(),
					NetworkService:             remoteDst.NetworkService,
					Context:                    remoteDst.GetContext(),
					Labels:                     remoteDst.GetLabels(),
					NetworkServiceEndpointName: endpoint.GetNetworkServiceEndpoint().GetName(),
					Path: common.Strings2Path(
						cce.model.GetNsm().GetName(),                  // src
						endpoint.GetNetworkServiceManager().GetName(), // dst
					),
				},
				MechanismPreferences: remoteMechanisms,
			}
		}
	}

	return &networkservice.NetworkServiceRequest{
		Connection: &networkservice.Connection{
			Id:                         "-",
			NetworkService:             requestConn.GetNetworkService(),
			Context:                    requestConn.GetContext(),
			Labels:                     requestConn.GetLabels(),
			NetworkServiceEndpointName: endpoint.GetNetworkServiceEndpoint().GetName(),
			Path: common.Strings2Path(
				cce.model.GetNsm().GetName(),                  // src
				endpoint.GetNetworkServiceManager().GetName(), // dst
			),
		},
		MechanismPreferences: remoteMechanisms,
	}
}

func (cce *endpointService) validateConnection(_ context.Context, conn *networkservice.Connection) error {
	if err := conn.IsComplete(); err != nil {
		return err
	}

	return nil
}

func (cce *endpointService) updateConnectionContext(ctx context.Context, source, destination *networkservice.Connection) error {
	if err := cce.validateConnection(ctx, destination); err != nil {
		return err
	}

	if err := source.UpdateContext(destination.GetContext()); err != nil {
		return err
	}

	return nil
}

func (cce *endpointService) updateConnectionParameters(nseConn *networkservice.Connection, endpoint *registry.NSERegistration) {
	if cce.nseManager.IsLocalEndpoint(endpoint) {
		modelEp := cce.model.GetEndpoint(endpoint.GetNetworkServiceEndpoint().GetName())
		if modelEp != nil { // In case of tests this could be empty
			nseConn.GetMechanism().GetParameters()[mechanismCommon.Workspace] = modelEp.Workspace
			nseConn.GetMechanism().GetParameters()[kernel.WorkspaceNSEName] = modelEp.Endpoint.GetNetworkServiceEndpoint().GetName()
		}
		logrus.Infof("NSM:(7.2.6.2.4) Update Local NSE connection parameters: %v", nseConn.Mechanism)
	}
}

// NewEndpointService -  creates a service to connect to endpoint
func NewEndpointService(nseManager unifiednsm.NetworkServiceEndpointManager, properties *properties.Properties, mdl model.Model) networkservice.NetworkServiceServer {
	return &endpointService{
		nseManager: nseManager,
		props:      properties,
		model:      mdl,
	}
}
