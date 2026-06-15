"""PromptNet Python adapter. Works with LangChain, LangGraph, or raw Python."""

import threading
import time

import grpc

from promptnet.v1 import prompt_pb2, prompt_pb2_grpc


def _subject(uri):
    return "promptnet." + uri.removeprefix("promptnet://").replace("/", ".")


class PromptClient:
    def __init__(
        self, host, token=None, tls=False, ca_cert=None, cache_ttl=0, nats_url=None
    ):
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
        self._nats_url = nats_url  # e.g. "nats://your-company.prompts.io:4222"

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

    def diff(self, uri, new_template):
        """Semantic Propagation Diff of the stored prompt at `uri` vs an edited
        template, computed server-side with the server's embedding model.
        Returns the DiffPromptResponse (a list of Change with the three signals).
        """
        return self._stub.DiffPrompt(
            prompt_pb2.DiffPromptRequest(uri=uri, new_template=new_template),
            metadata=self._md,
        )

    def subscribe(self, uri, on_change):
        """Register as a subscriber: call on_change(version_hash) whenever the
        prompt at `uri` is republished (push). TTL polling via cache_ttl is the
        pull side. Returns the daemon thread running the NATS subscription.

        Requires `pip install nats-py` and nats_url set on the client.
        """
        if not self._nats_url:
            raise ValueError("set nats_url on PromptClient to subscribe")

        import asyncio

        import nats

        subject = _subject(uri)

        def run():
            async def main():
                nc = await nats.connect(self._nats_url)

                async def handler(msg):
                    self._cache.pop(uri, None)  # drop stale cache, force refresh
                    on_change(msg.data.decode())

                await nc.subscribe(subject, cb=handler)
                while True:
                    await asyncio.sleep(3600)

            asyncio.run(main())

        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t

    def close(self):
        self._chan.close()
