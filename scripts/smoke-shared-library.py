"""Exercise the release ABI in an isolated directory, without credentials or model calls."""

import argparse
import base64
import ctypes
import json
import os
import tempfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("library")
    parser.add_argument("version")
    args = parser.parse_args()
    library = ctypes.CDLL(os.path.abspath(args.library))

    class Buffer(ctypes.Structure):
        _fields_ = [("ptr", ctypes.c_void_p), ("length", ctypes.c_size_t)]

    library.cliproxyPluginCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buffer)]
    library.cliproxyPluginCall.restype = ctypes.c_int
    library.cliproxyPluginFree.argtypes = [ctypes.c_void_p, ctypes.c_size_t]
    library.cliproxyPluginFree.restype = None

    def call(method, request=None):
        payload = json.dumps(request or {}).encode()
        response = Buffer()
        code = library.cliproxyPluginCall(method.encode(), payload, len(payload), ctypes.byref(response))
        assert code == 0 and response.ptr
        try:
            envelope = json.loads(ctypes.string_at(response.ptr, response.length))
        finally:
            library.cliproxyPluginFree(response.ptr, response.length)
        assert envelope["ok"], envelope.get("error")
        return envelope["result"]

    with tempfile.TemporaryDirectory(prefix="candy-abi-smoke-") as work:
        previous = os.getcwd()
        try:
            os.chdir(work)
            os.mkdir("plugins")
            metadata = call("plugin.register")["metadata"]
            assert metadata["Version"] == args.version
            routes = call("management.register")["routes"]
            base = "/v0/management/plugins/cpa-codex-candy-eval"
            assert any(r["Path"] == base + "/automation" for r in routes)
            response = call("management.handle", {"Method": "GET", "Path": base + "/automation"})
            assert response["StatusCode"] == 200
            state = json.loads(base64.b64decode(response["Body"]))
            assert state["config"]["enabled"] is False
            assert state["config"]["auto_disable"] is False
            assert state["config"]["interval_minutes"] == 30
            call("plugin.quiesce")
            print(json.dumps({"version": args.version, "abi_smoke": "passed", "production_state_touched": False}))
        finally:
            os.chdir(previous)


if __name__ == "__main__":
    main()
