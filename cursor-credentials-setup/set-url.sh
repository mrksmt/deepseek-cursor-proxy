python3 <<'PY'
import sqlite3, json, shutil
from datetime import datetime
from pathlib import Path
db = Path.home() / "Library/Application Support/Cursor/User/globalStorage/state.vscdb"
backup = db.with_suffix(f".vscdb.bak-{datetime.now():%Y%m%d-%H%M%S}")
shutil.copy2(db, backup)
print("backup:", backup)
# <<< СВОЙ URL >>>
NEW_BASE_URL = "https://stegosaur-unreached-unvaried.ngrok-free.dev/v1"
RESET_KEY_SECRET = True  # True = сбросить encrypted key, чтобы форма снова приняла ввод
key = "src.vs.platform.reactivestorage.browser.reactiveStorageServiceImpl.persistentStorage.applicationUser"
con = sqlite3.connect(db)
cur = con.cursor()
raw = cur.execute("SELECT value FROM ItemTable WHERE key=?", (key,)).fetchone()
if not raw:
    raise SystemExit("reactive storage key not found")
data = json.loads(raw[0])
print("before openAIBaseUrl:", data.get("openAIBaseUrl"))
print("before useOpenAIKey:", data.get("useOpenAIKey"))
data["openAIBaseUrl"] = NEW_BASE_URL
data["useOpenAIKey"] = True
cur.execute("UPDATE ItemTable SET value=? WHERE key=?", (json.dumps(data, ensure_ascii=False), key))
if RESET_KEY_SECRET:
    cur.execute("DELETE FROM ItemTable WHERE key=?", ("secret://cursorAuth/openAIKey",))
    print("deleted secret://cursorAuth/openAIKey")
con.commit()
con.close()
print("after openAIBaseUrl:", NEW_BASE_URL)
print("done")
PY