import base64
import json
import os
import subprocess
import sys
import time
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parents[3]
PLUGIN_DIR = ROOT / "integrations" / "dify-plugin" / "opensandbox"
TEMPLATE_PATH = Path(__file__).resolve().parent / "workflow_template.yml"


def wait_for_ok(url: str, timeout: int = 300) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            resp = requests.get(url, timeout=5)
            if resp.status_code == 200:
                return
        except Exception:
            pass
        time.sleep(2)
    raise RuntimeError(f"Timed out waiting for {url}")


def get_csrf_token(session: requests.Session) -> str:
    print(f"Cookies after login: {[c.name for c in session.cookies]}")
    for cookie in session.cookies:
        if "csrf" in cookie.name.lower():
            print(f"Found CSRF token in cookie: {cookie.name}")
            return cookie.value
    # Some Dify versions may not require CSRF token
    print("Warning: CSRF token cookie not found, continuing without it")
    return ""


def setup_and_login(session: requests.Session, base_url: str, email: str, password: str) -> str:
    setup_resp = session.get(f"{base_url}/console/api/setup", timeout=10)
    setup_status = setup_resp.json()
    print(f"Setup status: {setup_status}")
    
    if setup_status.get("step") == "not_started":
        print("Running initial setup...")
        resp = session.post(
            f"{base_url}/console/api/setup",
            json={
                "email": email,
                "name": "OpenSandbox E2E",
                "password": password,
                "language": "en-US",
            },
            timeout=20,
        )
        print(f"Setup response: {resp.status_code} {resp.text[:200] if resp.text else ''}")
        if resp.status_code not in {200, 201}:
            raise RuntimeError(f"Setup failed: {resp.status_code} {resp.text}")
        # Wait a bit for setup to complete
        time.sleep(2)

    # Try both encoded and plain password
    encoded_password = base64.b64encode(password.encode("utf-8")).decode("utf-8")
    print(f"Logging in with email: {email}")
    
    # First try with base64 encoded password (older Dify versions)
    login_resp = session.post(
        f"{base_url}/console/api/login",
        json={"email": email, "password": encoded_password},
        timeout=10,
    )
    print(f"Login response (encoded): {login_resp.status_code}")
    
    # If failed, try with plain password (newer Dify versions)
    if login_resp.status_code != 200:
        login_resp = session.post(
            f"{base_url}/console/api/login",
            json={"email": email, "password": password},
            timeout=10,
        )
        print(f"Login response (plain): {login_resp.status_code}")
    
    if login_resp.status_code != 200:
        raise RuntimeError(f"Login failed: {login_resp.status_code} {login_resp.text}")
    
    login_data = login_resp.json()
    print(f"Login data keys: {list(login_data.keys())}")

    return get_csrf_token(session)


def start_plugin(session: requests.Session, base_url: str, csrf_token: str) -> subprocess.Popen:
    headers = {"X-CSRF-Token": csrf_token} if csrf_token else {}
    resp = session.get(f"{base_url}/console/api/workspaces/current/plugin/debugging-key", headers=headers, timeout=10)
    if resp.status_code != 200:
        raise RuntimeError(f"Failed to get debugging key: {resp.status_code} {resp.text}")
    data = resp.json()
    remote_url = f"{data['host']}:{data['port']}"
    remote_key = data["key"]

    env = os.environ.copy()
    env["INSTALL_METHOD"] = "remote"
    env["REMOTE_INSTALL_URL"] = remote_url
    env["REMOTE_INSTALL_KEY"] = remote_key

    print(f"Starting plugin with REMOTE_INSTALL_URL={remote_url}")

    return subprocess.Popen(
        [sys.executable, "-m", "main"],
        cwd=str(PLUGIN_DIR),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )


def wait_for_plugin(session: requests.Session, base_url: str, csrf_token: str, name: str, timeout: int = 120) -> None:
    headers = {"X-CSRF-Token": csrf_token} if csrf_token else {}
    deadline = time.time() + timeout
    while time.time() < deadline:
        resp = session.get(f"{base_url}/console/api/workspaces/current/plugin/list", headers=headers, timeout=10)
        if resp.status_code == 200:
            plugins = resp.json().get("plugins", [])
            if any(p.get("name") == name for p in plugins):
                return
        time.sleep(2)
    raise RuntimeError(f"Plugin {name} not found after waiting")


def ensure_provider_credentials(
    session: requests.Session,
    base_url: str,
    csrf_token: str,
    provider: str,
    provider_type: str,
    base_url_value: str,
    api_key: str,
) -> str:
    headers = {"X-CSRF-Token": csrf_token}
    credentials_payload = {
        "opensandbox_base_url": base_url_value,
        "opensandbox_api_key": api_key,
    }
    
    # Determine provider type based on name format (UUID/name/name pattern = plugin)
    is_plugin = "/" in provider
    
    # First, try to create a credential
    if is_plugin:
        # For plugin providers, use the add credential endpoint
        add_url = f"{base_url}/console/api/workspaces/current/tool-provider/builtin/{provider}/credentials/add"
    else:
        add_url = f"{base_url}/console/api/workspaces/current/tool-provider/builtin/{provider}/credentials/add"
    
    # Payload for adding credentials
    add_payload = {
        "credentials": credentials_payload,
        "type": "builtin",
    }
    
    print(f"Adding credentials via: {add_url}")
    add_resp = session.post(add_url, headers=headers, json=add_payload, timeout=10)
    print(f"Add credentials response: {add_resp.status_code} {add_resp.text[:300] if add_resp.text else ''}")
    
    # If add fails, try different approaches
    if add_resp.status_code not in {200, 201}:
        # Try with type=api
        add_payload["type"] = "api"
        print(f"Retrying with type=api...")
        add_resp = session.post(add_url, headers=headers, json=add_payload, timeout=10)
        print(f"Add credentials response: {add_resp.status_code} {add_resp.text[:300] if add_resp.text else ''}")
    
    if add_resp.status_code not in {200, 201}:
        # Try direct update on provider
        update_url = f"{base_url}/console/api/workspaces/current/tool-provider/builtin/{provider}/credentials"
        print(f"Trying PUT to: {update_url}")
        update_resp = session.put(update_url, headers=headers, json={"credentials": credentials_payload}, timeout=10)
        print(f"PUT credentials response: {update_resp.status_code} {update_resp.text[:300] if update_resp.text else ''}")
        if update_resp.status_code in {200, 201}:
            add_resp = update_resp
    
    if add_resp.status_code not in {200, 201, 400, 404}:
        raise RuntimeError(f"Failed to add credentials: {add_resp.status_code} {add_resp.text}")
    
    # For plugins, credentials might be set directly without needing to fetch
    if add_resp.status_code in {200, 201}:
        # Try to get credential ID from response
        try:
            resp_data = add_resp.json()
            if isinstance(resp_data, dict) and "id" in resp_data:
                return resp_data["id"]
        except Exception:
            pass
        # Return provider name as credential ID for plugins
        return provider

    # Fallback: try to list credentials
    cred_url = f"{base_url}/console/api/workspaces/current/tool-provider/builtin/{provider}/credentials"
    cred_resp = session.get(cred_url, headers=headers, timeout=10)
    print(f"List credentials response: {cred_resp.status_code} {cred_resp.text[:200] if cred_resp.text else ''}")
    
    if cred_resp.status_code != 200:
        # For plugins, credential might be set at provider level
        print("Credentials API not available, using provider as credential ID")
        return provider

    creds = cred_resp.json()
    if isinstance(creds, dict) and "credentials" in creds:
        creds = creds["credentials"]
    if not isinstance(creds, list) or not creds:
        print("No credentials in list, using provider as credential ID")
        return provider
    return creds[0]["id"]


def fetch_tool_provider(session: requests.Session, base_url: str, csrf_token: str, provider_name: str, timeout: int = 60) -> dict:
    headers = {"X-CSRF-Token": csrf_token} if csrf_token else {}
    deadline = time.time() + timeout
    
    while time.time() < deadline:
        resp = session.get(f"{base_url}/console/api/workspaces/current/tool-providers", headers=headers, timeout=10)
        if resp.status_code != 200:
            raise RuntimeError(f"Failed to list tool providers: {resp.status_code} {resp.text}")
        
        providers = resp.json()
        # Debug: print available provider info
        for p in providers[:5]:  # First 5 providers for debugging
            if isinstance(p, dict):
                print(f"  Provider: name={p.get('name')}, plugin_id={p.get('plugin_id')}, type={p.get('type')}")
        
        for provider in providers:
            if not isinstance(provider, dict):
                continue
            name = provider.get("name") or ""
            plugin_id = provider.get("plugin_id") or ""
            
            # Try matching by name
            if name == provider_name:
                return provider
            # Also check if name contains provider_name (plugin providers may have prefixes like uuid/name/name)
            if provider_name in name:
                print(f"Found provider by partial name match: {name}")
                return provider
            # Also check plugin_id for plugin-based providers
            if plugin_id and provider_name in plugin_id:
                print(f"Found provider by plugin_id: {plugin_id}")
                return provider
        
        print(f"Provider {provider_name} not found yet, waiting...")
        time.sleep(3)
    
    raise RuntimeError(f"Provider {provider_name} not found in tool providers list after {timeout}s")


def render_workflow(template: str, replacements: dict) -> str:
    for key, value in replacements.items():
        template = template.replace(key, value)
    return template


def import_workflow(session: requests.Session, base_url: str, csrf_token: str, yaml_content: str) -> str:
    headers = {"X-CSRF-Token": csrf_token}
    resp = session.post(
        f"{base_url}/console/api/apps/imports",
        headers=headers,
        json={"mode": "yaml-content", "yaml_content": yaml_content},
        timeout=20,
    )
    if resp.status_code not in {200, 201, 202}:
        raise RuntimeError(f"Import failed: {resp.status_code} {resp.text}")

    data = resp.json()
    if data.get("status") == "pending":
        confirm = session.post(
            f"{base_url}/console/api/apps/imports/{data['id']}/confirm",
            headers=headers,
            timeout=20,
        )
        if confirm.status_code not in {200, 201}:
            raise RuntimeError(f"Import confirm failed: {confirm.status_code} {confirm.text}")
        data = confirm.json()
    app_id = data.get("app_id")
    if not app_id:
        raise RuntimeError(f"Import did not return app_id: {data}")
    return app_id


def run_workflow(session: requests.Session, base_url: str, csrf_token: str, app_id: str) -> str:
    headers = {"X-CSRF-Token": csrf_token}
    resp = session.post(
        f"{base_url}/console/api/apps/{app_id}/workflows/draft/run",
        headers=headers,
        json={"inputs": {}},
        stream=True,
        timeout=30,
    )
    if resp.status_code != 200:
        raise RuntimeError(f"Workflow run failed: {resp.status_code} {resp.text}")

    output_buffer = []
    start = time.time()
    for line in resp.iter_lines(decode_unicode=True):
        if not line:
            continue
        if line.startswith("data:"):
            payload = line[5:].strip()
            output_buffer.append(payload)
        if time.time() - start > 90:
            break
    return "\n".join(output_buffer)


def main() -> None:
    dify_port = os.environ.get("DIFY_PORT", "5001")
    default_base_url = f"http://localhost:{dify_port}"
    base_url = os.environ.get("DIFY_CONSOLE_API_URL", default_base_url)
    email = os.environ.get("DIFY_ADMIN_EMAIL", "admin@example.com")
    password = os.environ.get("DIFY_ADMIN_PASSWORD", "ChangeMe123!")
    opensandbox_url = os.environ.get("OPEN_SANDBOX_BASE_URL", "http://localhost:8080")
    opensandbox_api_key = os.environ.get("OPEN_SANDBOX_API_KEY", "opensandbox-e2e-key")

    print(f"Configuration:")
    print(f"  DIFY_CONSOLE_API_URL: {base_url}")
    print(f"  OPEN_SANDBOX_BASE_URL: {opensandbox_url}")
    print(f"  DIFY_ADMIN_EMAIL: {email}")

    print(f"\nWaiting for Dify API at {base_url}/console/api/ping ...")
    wait_for_ok(f"{base_url}/console/api/ping", timeout=300)
    print("Dify API is ready")

    print(f"\nWaiting for OpenSandbox at {opensandbox_url}/health ...")
    wait_for_ok(f"{opensandbox_url}/health", timeout=120)
    print("OpenSandbox is ready")

    print("\nSetting up Dify admin account...")
    session = requests.Session()
    csrf_token = setup_and_login(session, base_url, email, password)
    print("Dify login successful")

    print("\nStarting plugin process...")
    plugin_proc = start_plugin(session, base_url, csrf_token)
    try:
        print("Waiting for plugin to register in Dify...")
        wait_for_plugin(session, base_url, csrf_token, "opensandbox", timeout=180)
        print("Plugin registered successfully")

        print("\nFetching tool provider info...")
        provider = fetch_tool_provider(session, base_url, csrf_token, "opensandbox")
        print(f"Provider found: {provider.get('name')}")

        # Debug: print full provider info
        print(f"\nProvider details: {json.dumps(provider, indent=2, default=str)[:1000]}")
        
        print("\nConfiguring OpenSandbox credentials...")
        provider_name = provider.get("name", "opensandbox")
        provider_type = provider.get("type", "builtin")
        credential_id = ensure_provider_credentials(
            session,
            base_url,
            csrf_token,
            provider=provider_name,
            provider_type=provider_type,
            base_url_value=opensandbox_url,
            api_key=opensandbox_api_key,
        )
        print(f"Credentials configured: {credential_id}")

        tools = {tool["name"]: tool for tool in provider.get("tools", [])}
        print(f"Available tools: {list(tools.keys())}")

        # Helper to get tool label with fallback
        def get_tool_label(tool_name: str, default: str) -> str:
            if tool_name in tools:
                return tools[tool_name].get("label", {}).get("en_US", default)
            return default

        # Get label from provider, handling nested structure
        provider_label = provider.get("label", {})
        if isinstance(provider_label, dict):
            provider_label_text = provider_label.get("en_US", provider["name"])
        else:
            provider_label_text = str(provider_label) if provider_label else provider["name"]

        replacements = {
            "__PROVIDER_ID__": provider["name"],
            "__PROVIDER_NAME__": provider_label_text,
            "__PLUGIN_UNIQUE_IDENTIFIER__": provider.get("plugin_unique_identifier") or provider.get("plugin_id") or "",
            "__CREDENTIAL_ID__": credential_id,
            "__TOOL_CREATE_LABEL__": get_tool_label("sandbox_create", "Create Sandbox"),
            "__TOOL_RUN_LABEL__": get_tool_label("sandbox_run", "Run Command"),
            "__TOOL_KILL_LABEL__": get_tool_label("sandbox_kill", "Kill Sandbox"),
        }
        print(f"Replacements: {replacements}")

        print("\nImporting workflow...")
        template = TEMPLATE_PATH.read_text(encoding="utf-8")
        workflow_yaml = render_workflow(template, replacements)
        app_id = import_workflow(session, base_url, csrf_token, workflow_yaml)
        print(f"Workflow imported: app_id={app_id}")

        print("\nRunning workflow...")
        output = run_workflow(session, base_url, csrf_token, app_id)
        print(f"Workflow output:\n{output[:500]}...")

        if "opensandbox-e2e" not in output:
            raise RuntimeError(f"Expected 'opensandbox-e2e' not found in output. Raw stream:\n{output}")

        print("\n" + "=" * 50)
        print("E2E TEST PASSED")
        print("=" * 50)
    finally:
        print("\nTerminating plugin process...")
        plugin_proc.terminate()
        try:
            plugin_proc.wait(timeout=5)
        except Exception:
            plugin_proc.kill()
        print("Plugin process terminated")


if __name__ == "__main__":
    main()
