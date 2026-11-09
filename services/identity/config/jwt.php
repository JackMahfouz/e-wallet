<?php

return [

    /*
    |--------------------------------------------------------------------------
    | Refresh Token Lifetime
    |--------------------------------------------------------------------------
    |
    | In minutes. Refresh tokens are stored server side, rotated on every use,
    | and expire after this long. Access tokens are issued by the auth service
    | (libs/auth), which owns their lifetime through its own AUTH_ACCESS_TTL.
    |
    */

    'refresh_ttl' => (int) env('JWT_REFRESH_TTL', 15),

    /*
    |--------------------------------------------------------------------------
    | Auth Service (gRPC, internal only)
    |--------------------------------------------------------------------------
    |
    | Address of the libs/auth server that issues and verifies access tokens,
    | and the deadline in seconds for each call. It must be reachable only
    | from inside the cluster.
    |
    */

    'auth_addr' => env('AUTH_RPC_ADDR', '127.0.0.1:9090'),

    'auth_timeout' => (float) env('AUTH_RPC_TIMEOUT', 2),

];
