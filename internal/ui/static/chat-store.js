/* Browser-owned writes survive tab closure. Never clear a send before a durable
   transaction commits, and never change its destination, identity or payload. */
(() => {
  "use strict";
  let opening;
  const destinationKey = dest => JSON.stringify([dest.node || "", dest.crew, dest.role]);
  function database() {
    if (!opening) opening = new Promise((resolve, reject) => {
      const request = indexedDB.open("zmina-chat", 2);
      request.onupgradeneeded = () => {
        const db=request.result;
        if(!db.objectStoreNames.contains("outbox")) db.createObjectStore("outbox", {keyPath:"key"});
        if(!db.objectStoreNames.contains("history")) db.createObjectStore("history");
        if(!db.objectStoreNames.contains("draft-files")) db.createObjectStore("draft-files");
      };
      request.onsuccess = () => { const db = request.result; db.onversionchange = () => { db.close(); opening = null; }; resolve(db); };
      request.onerror = () => { opening = null; reject(request.error); };
      request.onblocked = () => { opening = null; reject(new Error("Close older zmina tabs, then retry.")); };
    });
    return opening;
  }
  async function transaction(name, mode, work) {
    const db = await database();
    return new Promise((resolve, reject) => {
      const tx = db.transaction(name, mode);
      let value;
      tx.oncomplete = () => resolve(value);
      tx.onabort = () => reject(tx.error || new Error("Browser storage transaction failed."));
      tx.onerror = () => {}; // onabort reports the terminal outcome.
      try { work(tx.objectStore(name), result => { value = result; }, error => { tx.abort(); reject(error); }); }
      catch (error) { tx.abort(); reject(error); }
    });
  }
  async function list() {
    return transaction("outbox", "readonly", (store, done) => { const request = store.getAll(); request.onsuccess = () => done(request.result.sort((a, b) => (a.order || 0) - (b.order || 0) || a.at.localeCompare(b.at) || a.key.localeCompare(b.key))); });
  }
  async function enqueue(dest, id, text, files = []) {
    const entry = {key: destinationKey(dest) + ":" + id, dest: {...dest}, id, text, at: new Date().toISOString(), state: "local", attempts: 0, next: 0, files};
    return transaction("outbox", "readwrite", (store, done, fail) => {
      const request = store.getAll();
      request.onsuccess = () => {
        const existing = request.result.find(row => row.key === entry.key);
        if (existing) { if (existing.text !== text || JSON.stringify((existing.files||[]).map(f=>[f.id,f.name,f.size])) !== JSON.stringify(files.map(f=>[f.id,f.name,f.size]))) { fail(new Error("Message ID already has different text.")); return; } done(existing); return; }
        if(files.length>4 || files.some(file=>file.size>8*1024*1024)) {fail(new Error("Attach up to four files, each at most 8 MiB."));return;}
        const storedBytes=request.result.reduce((n,row)=>n+(row.files||[]).reduce((s,file)=>s+file.size,0),0);
        if(storedBytes+files.reduce((n,file)=>n+file.size,0)>64*1024*1024) {fail(new Error("Saved attachments are full. Wait for delivery before sending more."));return;}
        if (request.result.length >= 128 || request.result.reduce((n, row) => n + row.text.length, 0) + text.length > 1 << 20) { fail(new Error("Saved message queue is full. Wait for delivery before sending more.")); return; }
        entry.order = request.result.reduce((n, row) => Math.max(n, row.order || 0), 0) + 1;
        store.add(entry); done(entry);
      };
    });
  }
  window.zminaChatStore = {
    destinationKey, list, enqueue,
    saveFiles:(dest, files)=>transaction("draft-files","readwrite",store=>store.put(files,destinationKey(dest))),
    loadFiles:dest=>transaction("draft-files","readonly",(store,done)=>{const request=store.get(destinationKey(dest));request.onsuccess=()=>done(request.result||[]);}),
    update: entry => transaction("outbox", "readwrite", store => {
      // Another tab may already have acknowledged this identity. Never resurrect it.
      const request = store.get(entry.key); request.onsuccess = () => { if (request.result) store.put(entry); };
    }),
    remove: key => transaction("outbox", "readwrite", store => store.delete(key)),
    loadHistory: dest => transaction("history", "readonly", (store, done) => { const request = store.get(destinationKey(dest)); request.onsuccess = () => done(request.result?.snapshot); }),
    saveHistory: (dest, snapshot) => transaction("history", "readwrite", store => {
      const rows = store.getAll(), keys = store.getAllKeys();
      keys.onsuccess = () => {
        const key = destinationKey(dest);
        const ordered = keys.result.map((key, index) => ({key, at: rows.result[index].savedAt})).filter(row => row.key !== key).sort((a, b) => b.at - a.at);
        const savedAt = Math.max(Date.now(), ...ordered.map(row => row.at + 1));
        store.put({snapshot, savedAt}, key);
        for (const row of ordered.slice(19)) store.delete(row.key);
      };
    })
  };
})();
