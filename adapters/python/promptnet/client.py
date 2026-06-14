"""PromptNet Python adapter. Works with LangChain, LangGraph, or raw Python."""

import grpc

from promptnet.v1 import prompt_pb2, prompt_pb2_grpc


class PromptClient:
    def __init__(self, host, token=None, tls=False, ca_cert=None):
        if tls:
            creds = grpc.ssl_channel_credentials(
                root_certificates=open(ca_cert, "rb").read() if ca_cert else None
            )
            self._chan = grpc.secure_channel(host, creds)
        else:
            self._chan = grpc.insecure_channel(host)
        self._stub = prompt_pb2_grpc.PromptServiceStub(self._chan)
        self._md = [("authorization", f"Bearer {token}")] if token else None

    def get(self, uri):
        """Fetch a prompt by promptnet:// URI. Returns the GetPromptResponse."""
        return self._stub.GetPrompt(
            prompt_pb2.GetPromptRequest(uri=uri), metadata=self._md
        )

    def close(self):
        self._chan.close()
