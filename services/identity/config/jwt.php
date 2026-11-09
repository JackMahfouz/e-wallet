<?php

return [

    /*
    |--------------------------------------------------------------------------
    | JWT Signing Secret
    |--------------------------------------------------------------------------
    |
    | Shared secret used to sign and verify HS256 access tokens. Every service
    | that verifies tokens must use the same value. Generate one with
    | `openssl rand -base64 48`.
    |
    */

    'secret' => env('JWT_SECRET'),

    /*
    |--------------------------------------------------------------------------
    | Token Lifetimes
    |--------------------------------------------------------------------------
    |
    | Both values are in minutes. The access token is a stateless JWT. The
    | refresh token is stored server side, rotated on every use, and expires
    | after "refresh_ttl" minutes.
    |
    */

    'ttl' => (int) env('JWT_TTL', 15),

    'refresh_ttl' => (int) env('JWT_REFRESH_TTL', 15),

];
