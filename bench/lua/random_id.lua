
math.randomseed(os.time())

local max_id = 1000000

function request()
    local id = math.random(1, max_id)
    return wrk.format(nil, wrk.path .. "?id=" .. id)
end
