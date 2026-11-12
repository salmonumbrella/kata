package sqlitestore_test

import (
 "encoding/json/v2"
 "os"
 "testing"

 "github.com/stretchr/testify/require"
 "go.kenn.io/kata/internal/db"
 "go.kenn.io/kata/internal/uid"
)
type relayProof struct { ProjectUID string `json:"project_uid"`; ReceiverUID string `json:"receiver_uid"`; RequestUID string `json:"request_uid"`; Pin db.RootKeyPin `json:"pin"`; Source db.RemoteEvent `json:"source"`; Binding string `json:"binding"`; RelayUID string `json:"relay_uid"`; Envelopes []db.RelayEnvelope `json:"envelopes"` }
func TestS5OldRelay(t *testing.T) {
 ctx:=t.Context(); b,e:=os.ReadFile("/tmp/kata-s5-relay.json"); require.NoError(t,e); var f relayProof; require.NoError(t,json.Unmarshal(b,&f)); relay:=openTestDB(t)
 p,e:=relay.CreateProjectWithUID(ctx,"relay-project",f.ProjectUID); require.NoError(t,e); require.NoError(t,relay.PinRootAuthority(ctx,f.Pin))
 _,e=relay.UpsertFederationBinding(ctx,db.FederationBinding{ProjectID:p.ID,Role:db.FederationRoleSpoke,HubURL:"https://daemon.example",HubProjectID:42,HubProjectUID:p.UID,Actor:"operator",PushEnabled:true,Enabled:true}); require.NoError(t,e)
 upstream,e:=uid.New(); require.NoError(t,e)
 _,e=relay.SetRelayBindingConfig(ctx,p.ID,db.RelayBindingConfig{ProtocolVersion:db.RelayProtocolVersion,BindingUID:upstream,UpstreamInstanceUID:f.Pin.AuthorityUID,AuthorityUID:f.Pin.AuthorityUID,HubPath:[]string{f.Pin.AuthorityUID,relay.InstanceUID()},LocalActor:"operator",ServeDownstream:true,ResetEpoch:1}); require.NoError(t,e)
 parent,_,e:=relay.CreateAPIToken(ctx,db.CreateAPITokenParams{Actor:"operator",AdminActor:"admin",PlaintextToken:"relay-proof-parent-token"}); require.NoError(t,e)
 grant,e:=relay.CreateRelayEnrollment(ctx,db.CreateRelayEnrollmentParams{ProjectID:p.ID,ParentTokenID:parent.ID,SpokeInstanceUID:f.ReceiverUID,ProtocolVersion:db.RelayProtocolVersion,Token:"relay-proof-child-token"}); require.NoError(t,e)
 body,e:=db.EncodeRelaySourceEvent(f.Source); require.NoError(t,e)
 envelope,e:=db.SealRelayEnvelope(db.RelayEnvelope{Version:db.RelayProtocolVersion,BindingUID:upstream,ProjectUID:p.UID,AuthorityUID:f.Pin.AuthorityUID,SenderInstanceUID:f.Pin.AuthorityUID,ReceiverInstanceUID:relay.InstanceUID(),Epoch:1,Sequence:1,Stream:db.RelayStreamEvent,Path:[]string{f.Pin.AuthorityUID},SourceUID:f.Source.EventUID,SourceHash:f.Source.ContentHash,Body:body}); require.NoError(t,e)
 _,e=relay.AcceptRelayDeliveries(ctx,upstream,db.RelayBatch{Stream:db.RelayStreamEvent,Envelopes:[]db.RelayEnvelope{envelope}}); require.NoError(t,e)
 offered,e:=relay.PendingRelayDeliveries(ctx,grant.Enrollment.RelayBindingUID,db.RelayStreamEvent,100); require.NoError(t,e); require.Len(t,offered,1); require.Equal(t,body,offered[0].Body); require.Equal(t,f.Source.EventUID,offered[0].SourceUID); require.Equal(t,f.Source.ContentHash,offered[0].SourceHash)
 f.Binding=grant.Enrollment.RelayBindingUID; f.RelayUID=relay.InstanceUID(); f.Envelopes=offered; b,e=json.Marshal(f); require.NoError(t,e); require.NoError(t,os.WriteFile("/tmp/kata-s5-relay.json",b,0600))
}
