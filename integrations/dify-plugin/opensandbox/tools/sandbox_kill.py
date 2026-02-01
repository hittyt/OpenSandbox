from __future__ import annotations

from collections.abc import Generator
from typing import Any

from dify_plugin import Tool
from dify_plugin.entities.tool import ToolInvokeMessage
from loguru import logger
from opensandbox.exceptions import SandboxException
from opensandbox.sync.manager import SandboxManagerSync

from tools.utils import build_connection_config


class SandboxKillTool(Tool):
    def _invoke(self, tool_parameters: dict[str, Any]) -> Generator[ToolInvokeMessage]:
        sandbox_id = tool_parameters.get("sandbox_id", "")
        if not sandbox_id:
            yield self.create_text_message("sandbox_id is required.")
            return

        config = build_connection_config(self.runtime.credentials)

        manager = None
        try:
            manager = SandboxManagerSync.create(connection_config=config)
            manager.kill_sandbox(sandbox_id)
            payload = {"ok": True, "sandbox_id": sandbox_id}
            yield self.create_variable_message("ok", True)
            yield self.create_variable_message("sandbox_id", sandbox_id)
            yield self.create_json_message(payload)
        except SandboxException as exc:
            logger.exception("OpenSandbox error")
            yield self.create_text_message(f"OpenSandbox error: {exc}")
        except Exception as exc:
            logger.exception("Unexpected error")
            yield self.create_text_message(f"Unexpected error: {exc}")
        finally:
            try:
                if manager is not None:
                    manager.close()
            except Exception:
                pass
