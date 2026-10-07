import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    m = json.loads(line)
    if "id" not in m: continue
    meth = m.get("method"); r = None
    if meth == "initialize":
        r = {"protocolVersion": m["params"].get("protocolVersion","2025-06-18"), "capabilities":{"tools":{}}, "serverInfo":{"name":"bridge","version":"0"}}
    elif meth == "tools/list":
        r = {"tools":[{"name":"echo","description":"Echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}
    elif meth == "tools/call":
        r = {"content":[{"type":"text","text":"echo:" + m["params"]["arguments"].get("text","")}]}
    elif meth == "ping":
        r = {}
    if r is None:
        out = {"jsonrpc":"2.0","id":m["id"],"error":{"code":-32601,"message":"nope"}}
    else:
        out = {"jsonrpc":"2.0","id":m["id"],"result":r}
    sys.stdout.write(json.dumps(out)+"\n"); sys.stdout.flush()
