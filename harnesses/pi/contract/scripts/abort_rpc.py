import json, os, subprocess, sys, time, threading
D = "$CONTRACTS"
R = D + "/evidence/pi-live"
CLI = D + "/dl/x/earendil-works-pi-coding-agent-1.0.4/package/dist/bundle/cli.js"
env = dict(os.environ, HOME=R + "/home", PI_CODING_AGENT_DIR=R + "/agent", PI_OFFLINE="1", PI_TELEMETRY="0")
p = subprocess.Popen(["node", CLI, "--mode", "rpc", "--no-session", "--provider", "mockchat", "--model", "m1"], cwd=R + "/work", env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
out = open(R + "/abort-transcript.jsonl", "w"); t0 = time.time()
def rd():
    for line in iter(p.stdout.readline, b""):
        out.write(json.dumps({"t": round(time.time() - t0, 2), "rec": json.loads(line)}) + "\n"); out.flush()
threading.Thread(target=rd, daemon=True).start()
def send(c):
    out.write(json.dumps({"t": round(time.time() - t0, 2), "in": c}) + "\n"); p.stdin.write((json.dumps(c) + "\n").encode()); p.stdin.flush()
send({"id": "p1", "type": "prompt", "message": "slow please"}); time.sleep(3)
send({"id": "x", "type": "prompt", "message": "while busy"}); time.sleep(1)
send({"id": "a1", "type": "abort"}); time.sleep(3)
p.stdin.close(); p.wait(timeout=30)
