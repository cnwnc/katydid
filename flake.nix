{
  description = "katydid: file-backed music library database";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system} = {
        katydid = pkgs.buildGoModule {
          pname = "katydid";
          version = "0.1.0";
          src = self;
          subPackages = [ "cmd/kat" "cmd/katyd" "cmd/katy-fetchd" "cmd/katy-discordd" ];
          env.CGO_ENABLED = "0";
          vendorHash = null;
          doCheck = true;
          meta = {
            description = "katydid music library daemon and cli";
            mainProgram = "katyd";
          };
        };
        default = self.packages.${system}.katydid;
      };

      devShells.${system}.default = pkgs.mkShell {
        packages = with pkgs; [
          go
          gopls
          ffmpeg
          jq
        ];
      };
    };
}
