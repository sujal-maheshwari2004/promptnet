"""PromptNet Python adapter. Works with LangChain, LangGraph, or raw Python."""

import time

import grpc

from promptnet.v1 import prompt_pb2, prompt_pb2_grpc


class PromptClient:
    def __init__(self, host, token=None, tls=False, ca_cert=None, cache_ttl=0):
        if tls:
            creds = grpc.ssl_channel_credentials(
                root_certificates=open(ca_cert, "rb").read() if ca_cert else None
            )
            self._chan = grpc.secure_channel(host, creds)
        else:
            self._chan = grpc.insecure_channel(host)
        self._stub = prompt_pb2_grpc.PromptServiceStub(self._chan)
        self._md = [("authorization", f"Bearer {token}")] if token else None
        # L1 (local) cache: uri -> (response, expiry). cache_ttl=0 disables it.
        # The server only returns validated prompts, so anything cached is valid.
        self._cache_ttl = cache_ttl
        self._cache = {}

    def get(self, uri):
        """Fetch a prompt by promptnet:// URI. Returns the GetPromptResponse."""
        if self._cache_ttl:
            hit = self._cache.get(uri)
            if hit and hit[1] > time.monotonic():
                return hit[0]
        resp = self._stub.GetPrompt(
            prompt_pb2.GetPromptRequest(uri=uri), metadata=self._md
        )
        if self._cache_ttl:
            self._cache[uri] = (resp, time.monotonic() + self._cache_ttl)
        return resp

    def close(self):
        self._chan.close()
