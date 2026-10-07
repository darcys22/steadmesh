"""Drive `pi --mode rpc` through a scripted sequence and log every stdout record."""
import json, os, subprocess, sys, threading, queue, time

D = "$CONTRACTS"
R = D + "/evidence/pi-live"
CLI = D + "/dl/x/earendil-works-pi-coding-agent-1.0.4/package/dist/bundle/cli.js"
OUT = open(R + "/rpc-transcript.jsonl", "w")

env = dict(os.environ, HOME=R + "/home", PI_CODING_AGENT_DIR=R + "/agent", PI_OFFLINE="1", PI_TELEMETRY="0")
args = ["node", CLI, "--mode", "rpc", "--session-dir", R + "/sessions", "--provider", "mockchat", "--model", "m1"]
p = subprocess.Popen(args, cwd=R + "/work", env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=open(R + "/rpc-stderr.txt", "w"))
q = queue.Queue()

def reader():
    for line in iter(p.stdout.readline, b""):
        rec = json.loads(line)
        OUT.write(json.dumps({"dir": "out", "rec": rec}) + "\n"); OUT.flush()
        q.put(rec)
    q.put(None)

threading.Thread(target=reader, daemon=True).start()

def send(cmd):
    OUT.write(json.dumps({"dir": "in", "rec": cmd}) + "\n"); OUT.flush()
    p.stdin.write((json.dumps(cmd) + "\n").encode()); p.stdin.flush()

def wait_for(pred, timeout=60):
    end = time.time() + timeout
    while time.time() < end:
        try:
            rec = q.get(timeout=end - time.time())
        except queue.Empty:
            break
        if rec is None:
            return None
        if pred(rec):
            return rec
    OUT.write(json.dumps({"dir": "note", "rec": "timeout"}) + "\n")
    return None

def call(cmd):
    send(cmd)
    return wait_for(lambda r: r.get("type") == "response" and r.get("id") == cmd.get("id"))

def prompt_and_settle(i, text):
    send({"id": i, "type": "prompt", "message": text})
    return wait_for(lambda r: r.get("type") == "agent_settled")

st = call({"id": "s1", "type": "get_state"})
prompt_and_settle("p1", "hello")
prompt_and_settle("p2", "usetool please")
call({"id": "m2", "type": "set_model", "provider": "mockresp", "modelId": "m1"})
prompt_and_settle("p3", "hello responses")
call({"id": "m3", "type": "set_model", "provider": "mockant", "modelId": "m1"})
prompt_and_settle("p4", "hello anthropic")
call({"id": "a1", "type": "abort"})
first = call({"id": "s2", "type": "get_state"})
call({"id": "n1", "type": "new_session"})
call({"id": "s3", "type": "get_state"})
if first and first.get("data", {}).get("sessionFile"):
    call({"id": "w1", "type": "switch_session", "sessionPath": first["data"]["sessionFile"]})
    call({"id": "s4", "type": "get_state"})
p.stdin.close()
p.wait(timeout=30)
OUT.write(json.dumps({"dir": "note", "rec": {"exit": p.returncode}}) + "\n")
