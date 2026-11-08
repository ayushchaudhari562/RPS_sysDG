
math.randomseed(os.time())

local N = 1000000
local s = 0.99

function init(args)
    wrk.method = "GET"
end

function request()
    local u = math.random()
    if u > 0.999999 then u = 0.999999 end
    local rank = math.floor(N * (math.pow(u, 3.5))) + 1
    if rank > N then rank = N end
    if rank < 1 then rank = 1 end

    return wrk.format(nil, wrk.path .. "?id=" .. rank)
end
