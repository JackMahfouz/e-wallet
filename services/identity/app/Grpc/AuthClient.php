<?php

namespace App\Grpc;

use App\Grpc\Auth\V1\IssueRequest;
use App\Grpc\Auth\V1\IssueResponse;
use App\Grpc\Auth\V1\VerifyRequest;
use App\Grpc\Auth\V1\VerifyResponse;
use Grpc\BaseStub;

/**
 * Client for the internal auth.v1.Auth service (libs/auth).
 *
 * Hand written because there is no PHP plugin to generate it; the contract lives in
 * libs/auth/proto/auth/v1/auth.proto. Each method returns a unary call: ->wait() yields
 * [response, status].
 */
class AuthClient extends BaseStub
{
    /**
     * @param  array<string, mixed>  $metadata
     * @param  array<string, mixed>  $options
     */
    public function Issue(IssueRequest $request, array $metadata = [], array $options = [])
    {
        return $this->_simpleRequest(
            '/auth.v1.Auth/Issue',
            $request,
            [IssueResponse::class, 'decode'],
            $metadata,
            $options,
        );
    }

    /**
     * @param  array<string, mixed>  $metadata
     * @param  array<string, mixed>  $options
     */
    public function Verify(VerifyRequest $request, array $metadata = [], array $options = [])
    {
        return $this->_simpleRequest(
            '/auth.v1.Auth/Verify',
            $request,
            [VerifyResponse::class, 'decode'],
            $metadata,
            $options,
        );
    }
}
