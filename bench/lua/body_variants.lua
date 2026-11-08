
math.randomseed(os.time())

wrk.method = "POST"
wrk.headers["Content-Type"] = "text/plain; charset=utf-8"

local base_chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=+;: "
local special_snippets = {
    "He said: \"High throughput engineering in Go is remarkable!\"\n",
    "Path: C:\\Program Files\\Go\\src\\database\\sql\r\n",
    "Unicode: 🚀 Rocket, 100€ Euro, 日本語, Español ñ á, ⚡ Lightning\n",
    "{\"tag\": \"metric\", \"valid\": true, \"quoted\": \"escaped \\\"text\\\"\"}\n",
    "Log message with multiple\nlines and tabs\t\t[DEBUG]\n",
}

local body_pool = {}
for i = 1, 100 do
    local target_len = math.random(64, 4096)
    local buf = {}
    local curr_len = 0

    while curr_len < target_len do
        if math.random() < 0.3 then
            local snip = special_snippets[math.random(1, #special_snippets)]
            table.insert(buf, snip)
            curr_len = curr_len + #snip
        else
            local chunk_len = math.min(64, target_len - curr_len)
            local s = ""
            for j = 1, chunk_len do
                local idx = math.random(1, #base_chars)
                s = s .. base_chars:sub(idx, idx)
            end
            table.insert(buf, s)
            curr_len = curr_len + chunk_len
        end
    end

    local final_body = table.concat(buf):sub(1, 4096)
    table.insert(body_pool, final_body)
end

function request()
    local idx = math.random(1, #body_pool)
    local body = body_pool[idx]
    return wrk.format(nil, "/code-write", nil, body)
end
