<?php

namespace App\Providers;

use App\Models\User;
use App\Services\JwtService;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Auth;
use Illuminate\Support\ServiceProvider;

class AppServiceProvider extends ServiceProvider
{
    /**
     * Register any application services.
     */
    public function register(): void
    {
        //
    }

    /**
     * Bootstrap any application services.
     */
    public function boot(): void
    {
        Auth::viaRequest('jwt', function (Request $request): ?User {
            $claims = $this->app->make(JwtService::class)->decode((string) $request->bearerToken());

            return $claims === null ? null : User::find($claims['sub'] ?? null);
        });
    }
}
