python3 <<'PY'
import sqlite3, json, shutil
from datetime import datetime
from pathlib import Path
db = Path.home() / "Library/Application Support/Cursor/User/globalStorage/state.vscdb"
shutil.copy2(db, db.with_suffix(f".vscdb.bak-{datetime.now():%Y%m%d-%H%M%S}"))
key = "src.vs.platform.reactivestorage.browser.reactiveStorageServiceImpl.persistentStorage.applicationUser"
con = sqlite3.connect(db)
cur = con.cursor()
data = json.loads(cur.execute("SELECT value FROM ItemTable WHERE key=?", (key,)).fetchone()[0])
# временно выключаем, URL не трогаем
data["useOpenAIKey"] = False
# URL оставь свой
print("URL:", data.get("openAIBaseUrl"))
cur.execute("UPDATE ItemTable SET value=? WHERE key=?", (json.dumps(data, ensure_ascii=False), key))
cur.execute("DELETE FROM ItemTable WHERE key=?", ("secret://cursorAuth/openAIKey",))
con.commit(); con.close()
print("useOpenAIKey=False, secret cleared")
PY