package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCloudGatewayDecodeDirectRPC(t *testing.T) {
	raw := []byte(`{"type":"client_message","client_id":"background","stream_id":"stream","seq_id":5,"cursor":"cursor","message":{"id":12,"method":"thread/resume","params":{"threadId":"chat","excludeTurns":true}}}`)
	got, ok := decodeCloudGatewayEnvelope(raw)
	if !ok || got.Kind != "client_message" || got.ClientID != "background" || got.StreamID != "stream" || got.Method != "thread/resume" || got.ThreadID != "chat" || string(got.ID) != "12" || got.Sequence != 5 {
		t.Fatalf("direct RPC identity was not retained: %#v, supported=%v", got, ok)
	}
	if string(got.Fields["cursor"]) != `"cursor"` || string(got.Message["params"]) != `{"threadId":"chat","excludeTurns":true}` {
		t.Fatal("envelope metadata or RPC parameters were lost")
	}
}

func TestCloudGatewayDecodeRejectedFrames(t *testing.T) {
	cases := map[string]string{
		"unknown":            `{"type":"pong","client_id":"background","stream_id":"stream","seq_id":1,"status":"active"}`,
		"segmented":          `{"type":"client_message_chunk","client_id":"background","stream_id":"stream","seq_id":1,"segment_id":0,"segment_count":1,"message_size_bytes":2,"message_chunk_base64":"e30="}`,
		"missing client":     `{"type":"client_message","stream_id":"stream","message":{"method":"thread/resume"}}`,
		"missing stream":     `{"type":"client_message","client_id":"background","message":{"method":"thread/resume"}}`,
		"control identity":   `{"type":"client_message","client_id":"private-secret\n","stream_id":"stream","message":{"method":"thread/resume"}}`,
		"invalid UTF-8":      "{\"type\":\"client_message\",\"client_id\":\"background\xff\",\"stream_id\":\"stream\",\"message\":{\"method\":\"thread/resume\"}}",
		"duplicate envelope": `{"type":"pong","type":"client_message","client_id":"background","stream_id":"stream","message":{"method":"thread/resume"}}`,
		"duplicate RPC":      `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"method":"thread/read","method":"thread/resume"}}`,
		"duplicate thread":   `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"method":"thread/resume","params":{"threadId":"a","threadId":"b"}}}`,
		"invalid thread":     `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"method":"thread/resume","params":{"threadId":42}}}`,
		"invalid id":         `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"id":true,"method":"thread/resume"}}`,
		"fractional id":      `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"id":1.2,"method":"thread/resume"}}`,
		"mixed request":      `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"id":"x","method":"thread/resume","result":{}}}`,
		"missing sequence":   `{"type":"server_message","client_id":"background","stream_id":"stream","message":{"id":"x","result":{}}}`,
		"invalid sequence":   `{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":-1,"message":{"id":"x","result":{}}}`,
		"overflow sequence":  `{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":18446744073709551616,"message":{"id":"x","result":{}}}`,
		"trailing document":  `{"type":"client_message","client_id":"background","stream_id":"stream","message":{"method":"thread/resume"}} {}`,
		"oversized":          strings.Repeat(" ", 153601),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := decodeCloudGatewayEnvelope([]byte(raw)); ok {
				t.Fatal("unsupported or invalid frame acquired gateway ownership")
			}
		})
	}
}

func TestCloudGatewayPrivateReplyPreservedSequence(t *testing.T) {
	for _, payload := range []string{`"result":{"status":"unsubscribed"}`, `"error":{"code":-32602,"message":"private-secret"}`} {
		raw := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":18446744073709551615,"cursor":"cursor","extension":{"keep":true},"message":{"id":"cq-harness/nonce/1",` + payload + `}}`)
		got, reply, ok := cloudGatewayPrivateReply(raw, "cq-harness/nonce/")
		if !ok || string(reply["id"]) != `"cq-harness/nonce/1"` || bytes.Contains(got, []byte("private-secret")) || bytes.Contains(got, []byte("cq-harness")) {
			t.Fatal("private reply escaped or was not delivered to its owner")
		}
		var frame map[string]json.RawMessage
		if json.Unmarshal(got, &frame) != nil || string(frame["type"]) != `"pong"` || string(frame["status"]) != `"active"` || string(frame["seq_id"]) != "18446744073709551615" || frame["message"] != nil || string(frame["cursor"]) != `"cursor"` || string(frame["extension"]) != `{"keep":true}` {
			t.Fatal("Pong replacement changed sequence or envelope metadata")
		}
		replay, _, _ := cloudGatewayPrivateReply(raw, "cq-harness/nonce/")
		if !bytes.Equal(replay, got) {
			t.Fatal("replayed private reply produced a different public envelope")
		}
	}
}

func TestCloudGatewayUnownedFramesRemainByteIdentical(t *testing.T) {
	for _, raw := range []string{
		` { "type":"server_message", "client_id":"background", "stream_id":"stream", "seq_id":2, "message":{"id":"cq-harness/other/1","result":{}} } `,
		`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":2,"message":{"method":"thread/status/changed","params":{"threadId":"sibling"}}}`,
		`{"type":"server_message_chunk","client_id":"background","stream_id":"stream","seq_id":2,"segment_id":0,"segment_count":1,"message_size_bytes":2,"message_chunk_base64":"e30="}`,
		`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":2,"message":{"id":"cq-harness/nonce/1","result":{},"error":{}}}`,
		`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":2,"message":{"id":"cq-harness/nonce/1","method":"thread/resume"}}`,
		`not JSON private-secret`,
	} {
		got, reply, ok := cloudGatewayPrivateReply([]byte(raw), "cq-harness/nonce/")
		if ok || reply != nil || !bytes.Equal(got, []byte(raw)) {
			t.Fatal("unowned or unsupported frame was modified")
		}
	}
	raw := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":2,"message":{"id":"cq-harness/nonce/1","result":{}}}`)
	for _, prefix := range []string{"", "cq-harness/", "cq-harness", "cq-harness/nonce", "cq-harness/nonce/1/"} {
		got, _, ok := cloudGatewayPrivateReply(raw, prefix)
		if ok || !bytes.Equal(got, raw) {
			t.Fatal("invalid owner prefix claimed a private reply")
		}
	}
}

func TestCloudGatewayPongWithInstalledReceiverSequenceRules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the installed remote-control receiver fixture")
	}
	private := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":2,"message":{"id":"cq-harness/nonce/1","result":{"status":"unsubscribed"}}}`)
	pong, _, ok := cloudGatewayPrivateReply(private, "cq-harness/nonce/")
	if !ok {
		t.Fatal("private reply was not replaced")
	}
	// The three methods below are from the installed 0.167.0-alpha.3.2 app.asar
	// receiver. The fixture supplies its collection/transport callbacks only.
	const fixture = `const assert=require("node:assert/strict");
class Receiver {
 constructor(){this.seenServerSeqId=null;this.pendingSegmentedSeqId=null;this.accepted=[];this.gaps=0;this.pongs=0;this.delivered=[];}
 acceptServerSeqId(e){return(null==this.seenServerSeqId||!(e<=this.seenServerSeqId))&&(this.seenServerSeqId=e,!0)}
 hasServerSeqGap(e){return null!=this.pendingSegmentedSeqId&&e>this.pendingSegmentedSeqId||null!=this.seenServerSeqId&&e>this.seenServerSeqId+1}
 handlePong(e){let t=this.findStream(e.env_id,e.stream_id);null==t||((t.markPongReceived(),"unknown"===e.status)?t.closeForStreamReconnect("remote control app-server stream became unknown"):t.hasServerSeqGap(e.seq_id)?this.replayStreamGap(t,e.seq_id,"remote control app-server stream sequence gap detected"):t.acceptServerSeqId(e.seq_id)&&this.recordAcceptedEnvelope(t,e))}
 findStream(){return this}markPongReceived(){this.pongs++}closeForStreamReconnect(){this.gaps++}replayStreamGap(){this.gaps++}recordAcceptedEnvelope(_,e){this.accepted.push(e.seq_id)}
 message(e){if(this.hasServerSeqGap(e.seq_id)){this.gaps++;return}if(this.acceptServerSeqId(e.seq_id)){this.delivered.push(e.message);this.recordAcceptedEnvelope(this,e)}}
}
const replacement=JSON.parse(process.argv[1]);const r=new Receiver();
r.message({seq_id:1,message:{id:"visible-1",result:{}}});
r.handlePong(replacement);r.handlePong(replacement);
r.message({seq_id:3,message:{method:"thread/status/changed",params:{threadId:"sibling"}}});
assert.deepEqual(r.accepted,[1,2,3]);assert.equal(r.gaps,0);assert.equal(r.delivered.length,2);
r.handlePong(replacement);assert.deepEqual(r.accepted,[1,2,3]);
const replay=new Receiver();replay.message({seq_id:1,message:{}});replay.handlePong(replacement);replay.message({seq_id:3,message:{}});assert.deepEqual(replay.accepted,[1,2,3]);
const missing=new Receiver();missing.message({seq_id:1,message:{}});missing.message({seq_id:3,message:{}});assert.equal(missing.gaps,1);
const unknown=new Receiver();unknown.handlePong({...replacement,status:"unknown"});assert.equal(unknown.gaps,1);`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, node, "-e", fixture, string(pong)).CombinedOutput(); err != nil {
		t.Fatalf("installed receiver sequence fixture failed: %v\n%s", err, output)
	}
}
