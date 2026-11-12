package sqlitestore_test

import (
 "crypto/ed25519"
 "encoding/json/jsontext"
 "encoding/json/v2"
 "os"
 "testing"

 "github.com/stretchr/testify/require"
 "go.kenn.io/kata/internal/cron"
 "go.kenn.io/kata/internal/db"
 "go.kenn.io/kata/internal/testfix"
 "go.kenn.io/kata/internal/uid"
)

type relayProof struct {
 ProjectUID string `json:"project_uid"`
 ReceiverUID string `json:"receiver_uid"`
 RequestUID string `json:"request_uid"`
 Pin db.RootKeyPin `json:"pin"`
 Source db.RemoteEvent `json:"source"`
 Binding string `json:"binding"`
 RelayUID string `json:"relay_uid"`
 Envelopes []db.RelayEnvelope `json:"envelopes"`
}
func proofRead(t *testing.T) relayProof { t.Helper(); b,e:=os.ReadFile("/tmp/kata-s5-relay.json"); require.NoError(t,e); var f relayProof; require.NoError(t,json.Unmarshal(b,&f)); return f }
func proofWrite(t *testing.T,f relayProof) { t.Helper(); b,e:=json.Marshal(f); require.NoError(t,e); require.NoError(t,os.WriteFile("/tmp/kata-s5-relay.json",b,0600)) }
func TestS5RelayProduce(t *testing.T) {
 ctx:=t.Context(); root:=openTestDB(t)
 receiver,e:=testfix.OpenSQLite(ctx,"/tmp/kata-s5-receiver.db"); require.NoError(t,e); receiverUID:=receiver.InstanceUID(); require.NoError(t,receiver.Close())
 p,e:=root.CreateProject(ctx,"root-project"); require.NoError(t,e)
 public,_,e:=ed25519.GenerateKey(nil); require.NoError(t,e)
 pin:=db.RootKeyPin{ProjectUID:p.UID,AuthorityUID:root.InstanceUID(),KeyID:db.RootPublicKeyID(public),PublicKey:public}
 definition:=cron.JobDefinition{Version:1,Kind:"job",Enabled:true,Trigger:cron.Trigger{Kind:"manual"},Action:cron.Action{Kind:"execute",Prompt:"Inspect"},Issue:&cron.IssuePolicy{Kind:"per-run",Title:"Result"},Overlap:"forbid",Catchup:"skip",Options:jsontext.Value(`{"inputs":{"count":{"type":"int"}}}`)}
 job,_,e:=root.PutCronJob(ctx,db.PutCronJob{ProjectID:p.ID,Name:"Inspect",Actor:"operator",Definition:definition}); require.NoError(t,e)
 id,e:=uid.New(); require.NoError(t,e)
 req,e:=root.RequestCron(ctx,db.RequestCronIn{ProjectID:p.ID,UID:id,Kind:db.CronRequestKindRun,TargetUID:job.UID,Actor:"operator",TTLSeconds:600,Inputs:map[string]jsontext.Value{"count":jsontext.Value("-0")}}); require.NoError(t,e)
 require.Contains(t,req.Event.Payload,"cron.request_inputs."+id)
 proofWrite(t,relayProof{ProjectUID:p.UID,ReceiverUID:receiverUID,RequestUID:id,Pin:pin,Source:db.RemoteEventFromStored(*req.Event)})
}
func TestS5RelayReceive(t *testing.T) {
 ctx:=t.Context(); f:=proofRead(t); receiver,e:=testfix.OpenSQLite(ctx,"/tmp/kata-s5-receiver.db"); require.NoError(t,e); defer receiver.Close()
 p,e:=receiver.CreateProjectWithUID(ctx,"spoke-project",f.ProjectUID); require.NoError(t,e); require.NoError(t,receiver.PinRootAuthority(ctx,f.Pin))
 _,e=receiver.UpsertFederationBinding(ctx,db.FederationBinding{ProjectID:p.ID,Role:db.FederationRoleSpoke,HubURL:"https://daemon.example",HubProjectID:42,HubProjectUID:p.UID,Actor:"operator",PushEnabled:true,Enabled:true}); require.NoError(t,e)
 _,e=receiver.SetRelayBindingConfig(ctx,p.ID,db.RelayBindingConfig{ProtocolVersion:db.RelayProtocolVersion,BindingUID:f.Binding,UpstreamInstanceUID:f.RelayUID,AuthorityUID:f.Pin.AuthorityUID,HubPath:[]string{f.Pin.AuthorityUID,f.RelayUID,receiver.InstanceUID()},LocalActor:"operator",ResetEpoch:1}); require.NoError(t,e)
 _,e=receiver.AcceptRelayDeliveries(ctx,f.Binding,db.RelayBatch{Stream:db.RelayStreamEvent,Envelopes:f.Envelopes}); require.NoError(t,e)
 rows,e:=receiver.ListCronRequests(ctx,db.CronRequestList{ProjectID:p.ID}); require.NoError(t,e); require.Len(t,rows,1); require.Equal(t,f.RequestUID,rows[0].UID); require.Equal(t,jsontext.Value("-0"),rows[0].Inputs["count"])
}
