package debianupdate

import (
	"errors"
	"github.com/BurntSushi/toml"
	"github.com/dedis/cothority/crypto"
	"github.com/dedis/cothority/log"
	"github.com/dedis/cothority/monitor"
	"github.com/dedis/cothority/sda"
	"github.com/dedis/cothority/services/swupdate"
	"github.com/dedis/cothority/services/timestamp"
	"os"
	"sort"
	"time"
)

func init() {
	sda.SimulationRegister("DebianUpdateOneClient", NewOneClientSimulation)
}

type oneClientSimulation struct {
	sda.SimulationBFTree
	Base                      int
	Height                    int
	NumberOfInstalledPackages int
	NumberOfPackagesInRepo    int
	Snapshots                 string // All the snapshots filenames
}

func NewOneClientSimulation(config string) (sda.Simulation, error) {
	es := &oneClientSimulation{Base: 2, Height: 10}
	_, err := toml.Decode(config, es)
	if err != nil {
		return nil, err
	}
	//log.SetDebugVisible(3)
	return es, nil
}

func (e *oneClientSimulation) Setup(dir string, hosts []string) (
	*sda.SimulationConfig, error) {

	sc := &sda.SimulationConfig{}
	e.CreateRoster(sc, hosts, 2000)
	err := e.CreateTree(sc)

	if err != nil {
		return nil, err
	}
	err = CopyDir(dir, e.Snapshots)

	if err != nil {
		return nil, err
	}
	return sc, nil
}

func (e *oneClientSimulation) Run(config *sda.SimulationConfig) error {
	// The cothority configuration
	size := config.Tree.Size()
	log.Lvl2("Size is:", size, "rounds:", e.Rounds)

	// check if the service is running and get an handle to it
	service, ok := config.GetService(ServiceName).(*DebianUpdate)
	if service == nil || !ok {
		log.Fatal("Didn't find service", ServiceName)
	}

	// create and setup a new timestamp client
	c := timestamp.NewClient()
	maxIterations := 0
	_, err := c.SetupStamper(config.Roster, time.Millisecond*250, maxIterations)
	if err != nil {
		return nil
	}

	// get the release and snapshots
	current_dir, err := os.Getwd()

	if err != nil {
		return nil
	}
	snapshot_files, err := GetFileFromType(current_dir+"/"+e.Snapshots, "Packages")
	if err != nil {
		return nil
	}
	release_files, err := GetFileFromType(current_dir+"/"+e.Snapshots, "Release")
	if err != nil {
		return nil
	}

	sort.Sort(snapshot_files)
	sort.Sort(release_files)

	repos := make(map[string]*RepositoryChain)
	releases := make(map[string]*Release)

	updateClient := NewClient(config.Roster)

	var round *monitor.TimeMeasure

	log.Lvl2("Loading repository files")
	for i, release_file := range release_files {
		log.Lvl1("Parsing repo file", release_file)

		// Create a new repository structure (not a new skipchain..!)
		repo, err := NewRepository(release_file, snapshot_files[i],
			"https://snapshots.debian.org", e.Snapshots, e.NumberOfPackagesInRepo)
		log.ErrFatal(err)
		log.Lvl1("Repository created with", len(repo.Packages), "packages")

		// Recover all the hashes from the repo
		hashes := make([]crypto.HashID, len(repo.Packages))
		for i, p := range repo.Packages {
			hashes[i] = crypto.HashID(p.Hash)
		}

		// Compute the root and the proofs
		root, proofs := crypto.ProofTree(HashFunc(), hashes)
		lengths := []int64{}
		for _, proof := range proofs {
			lengths = append(lengths, int64(len(proof)))
		}
		// Store the repo, root and proofs in a release
		release := &Release{repo, root, proofs, lengths}

		// check if the skipchain has already been created for this repo
		sc, knownRepo := repos[repo.GetName()]

		if knownRepo {
			//round = monitor.NewTimeMeasure("add_to_skipchain")

			log.Lvl1("A skipchain for", repo.GetName(), "already exists",
				"trying to add the release to the skipchain.")

			// is the new block different ?
			// who should take care of that ? the client or the server ?
			// I would say the server, when it receive a new release
			// it should check that it's different than the actual release
			urr, err := service.UpdateRepository(nil,
				&UpdateRepository{sc, release})

			if err != nil {
				log.Lvl1(err)
			} else {

				// update the references to the latest block and release
				repos[repo.GetName()] = urr.(*UpdateRepositoryRet).RepositoryChain
				releases[repo.GetName()] = release
			}
		} else {
			//round = monitor.NewTimeMeasure("create_skipchain")

			log.Lvl2("Creating a new skipchain for", repo.GetName())

			cr, err := service.CreateRepository(nil,
				&CreateRepository{config.Roster, release, e.Base, e.Height})
			if err != nil {
				return err
			}

			// update the references to the latest block and release
			repos[repo.GetName()] = cr.(*CreateRepositoryRet).RepositoryChain
			releases[repo.GetName()] = release
		}
	}
	log.Lvl2("Loading repository files - done")

	latest_release_update := monitor.NewTimeMeasure("client_receive_latest_release")
	bw_update := monitor.NewCounterIOMeasure("client_bw_debianupdate", updateClient)
	lr, err := updateClient.LatestRelease(e.Snapshots)
	if err != nil {
		log.Lvl1(err)
		return nil
	}
	bw_update.Record()
	latest_release_update.Record()

	// Check signature on root

	verify_sig := monitor.NewTimeMeasure("verify_signature")
	log.Lvl1("Verifying root signature")
	if err := lr.Update[0].VerifySignatures(); err != nil {
		log.Lvl1("Failed verification of root's signature")
		return err
	}
	verify_sig.Record()

	// Verify proofs for installed packages
	round = monitor.NewTimeMeasure("verify_proofs")

	// take e.NumberOfInstalledPackages randomly insteand of the first

	log.Lvl1("Verifying at most", e.NumberOfInstalledPackages, "packages")
	i := 1
	for name, p := range lr.Packages {
		hash := []byte(p.Hash)
		proof := p.Proof
		if proof.Check(HashFunc(), lr.RootID, hash) {
			log.Lvl1("Package", name, "correctly verified")
		} else {
			log.ErrFatal(errors.New("The proof for " + name + " is not correct."))
		}
		i = i + 1
		if i > e.NumberOfInstalledPackages {
			break
		}
	}
	round.Record()

	// APT Emulation
	key := swupdate.NewPGP()
	signedFile := `Origin: Debian
Label: Debian
Suite: stable
Version: 8.7
Codename: jessie
Date: Sat, 14 Jan 2017 11:03:32 UTC
Architectures: amd64 arm64 armel armhf i386 mips mipsel powerpc ppc64el s390x
Components: main contrib non-free
Description: Debian 8.7 Released 14 January 2017
MD5Sum:
 9fbd46c44adcfaf32472545f81f7abda  1194094 contrib/Contents-amd64
 4aa54fa77079ba5bd9b0e3fc0eada87a    88313 contrib/Contents-amd64.gz
 4cc133343e50436e47c9df9a425279c0  1021028 contrib/Contents-arm64
 07a1b3cf03b0b8fe0fdce13ff1802d8c    72294 contrib/Contents-arm64.gz
 4be80045753e3163340641d1e990b35b  1035060 contrib/Contents-armel
 bb026464f56bf9199ec0aa7d677cbdf0    73507 contrib/Contents-armel.gz
 f7e9a5ab4cdca4dc1c15d9f7cc582b5e  1027963 contrib/Contents-armhf
 3012919fe0c4d9bd59734b15915496d0    73418 contrib/Contents-armhf.gz
 9257f2b683b0199649f62734b30e5b88  1190658 contrib/Contents-i386
 1f9f2d52ce6889b2c236a251d003ef63    87973 contrib/Contents-i386.gz
 f4b9cb08c5d62057f477dabb728f503e  1036779 contrib/Contents-mips
 f853009f65253938035bcd9b813d7b41    73599 contrib/Contents-mips.gz
 97678af601e0b0a4023c71170381beda  1036785 contrib/Contents-mipsel
 2e5b687999cf2fde50fb2aa575ae37f9    73704 contrib/Contents-mipsel.gz
 ad3d7fbd05b18244f5e522746527599d  1037497 contrib/Contents-powerpc
 4b6aa55e57ffb95a68508bf889488297    73868 contrib/Contents-powerpc.gz
 c1e98feb506a8da26f773d624dfccb80  1017446 contrib/Contents-ppc64el
 4ac6a8370ed8c646c8ddaac7e14933e5    71910 contrib/Contents-ppc64el.gz
 6bc22dc44e8d648f28aa4c84574e7d94  1032989 contrib/Contents-s390x
 f2cad8ef27b6b601300ad3c65f2a326f    73259 contrib/Contents-s390x.gz
 f5c46044f1f12965f20fe901d675d210  3015028 contrib/Contents-source
 4d1b78d8bbf50722b907a58495a9400e   334510 contrib/Contents-source.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-amd64
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-amd64.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-arm64
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-arm64.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-armel
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-armel.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-armhf
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-armhf.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-i386
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-i386.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-mips
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-mips.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-mipsel
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-mipsel.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-powerpc
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-powerpc.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-ppc64el
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-ppc64el.gz
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/Contents-udeb-s390x
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/Contents-udeb-s390x.gz
 3ef7af3110177294340242e08f84be84    84184 contrib/binary-all/Packages
 bbeb304c7bc904355f0ebc7fcd0438e8    27124 contrib/binary-all/Packages.gz
 628e9068aa73ee22bdcad2c5735e6fa0    24000 contrib/binary-all/Packages.xz
 b983f5ec0ed51034cc113f1805c67953       95 contrib/binary-all/Release
 eb1e578a9ce374f8c85f6bd9e089b53a   197960 contrib/binary-amd64/Packages
 cfe9a87d9eb25c4504fbdd98df005a39    59500 contrib/binary-amd64/Packages.gz
 2cd97854cac9aa0c578734dabf4d0178    50168 contrib/binary-amd64/Packages.xz
 13849fb6c24cf98a16021fd4bd62a7b1       97 contrib/binary-amd64/Release
 6bb5209484e88ce1b8112b0bc7b859c4   133508 contrib/binary-arm64/Packages
 cb545023235c40e6b0611411024da549    41575 contrib/binary-arm64/Packages.gz
 4e6bacfec4938a50f9b22d3aba22a65e    35840 contrib/binary-arm64/Packages.xz
 5768c227cffe85bf92cf352e887df0c5       97 contrib/binary-arm64/Release
 f5f1e58eb790e5e9d8ad7674e291a07c   139204 contrib/binary-armel/Packages
 8062fb030adcb6d1b9a0166f1350be44    43258 contrib/binary-armel/Packages.gz
 ba424d405fb68d460c50e307bc608e41    37100 contrib/binary-armel/Packages.xz
 6b2e7091b1d0c922ad5a523baccace57       97 contrib/binary-armel/Release
 141ce01bc013e5e3a2040ae028004fe6   144473 contrib/binary-armhf/Packages
 2b1c05fcd8e7967d23560442d36727a0    44700 contrib/binary-armhf/Packages.gz
 77574722a6fe6b5611b9494ddbffb82c    38136 contrib/binary-armhf/Packages.xz
 d8e7323ed580417ea00761ea0967d21a       97 contrib/binary-armhf/Release
 136058fcb040b276bf5778f14c878ca4   194806 contrib/binary-i386/Packages
 d628a04f7641ea52bfedf677cec60911    58600 contrib/binary-i386/Packages.gz
 9ffb0c4e398236e666f181a6fc8139fb    49492 contrib/binary-i386/Packages.xz
 dffa6d06c6a8f50d752b5458d2caa599       96 contrib/binary-i386/Release
 35c4c904acc518d5878acc1b027fc0ac   140865 contrib/binary-mips/Packages
 50f32e6275eaa69a14ad504233b7cdda    44000 contrib/binary-mips/Packages.gz
 e9e990d4934f4e32db40c3c4a7a4e7f2    37540 contrib/binary-mips/Packages.xz
 c63d261437c8e9eb323c8fffe318b4b6       96 contrib/binary-mips/Release
 21ef5e1dcac2202f99dc8d26dc806079   141128 contrib/binary-mipsel/Packages
 610fb9c24705b0c3e5fdd977eed82a32    43785 contrib/binary-mipsel/Packages.gz
 efa7b904df563c10d66b2a41847a78c5    37568 contrib/binary-mipsel/Packages.xz
 fc6dd5bf12e345a87c5ad0a59b58e247       98 contrib/binary-mipsel/Release
 92a23f029f8f5b520677a275e90cd152   141881 contrib/binary-powerpc/Packages
 6abcadff70446ff67cbf1030af03c7f0    43955 contrib/binary-powerpc/Packages.gz
 949757d0bac2cda2d0b463ce919c12e3    37700 contrib/binary-powerpc/Packages.xz
 c99cf261763db1fc1739292ae2215d2c       99 contrib/binary-powerpc/Release
 2077d1628f5b0f19c550aa12915cd031   132753 contrib/binary-ppc64el/Packages
 dd06674baf4a2b35fbb5d7790b016da7    41486 contrib/binary-ppc64el/Packages.gz
 89b34cca16b034fc85ac90ae0606935b    35696 contrib/binary-ppc64el/Packages.xz
 55c611ed63bc4c23810487378a788e20       99 contrib/binary-ppc64el/Release
 54acb2682782bea33fbd4abc5feb1d02   137757 contrib/binary-s390x/Packages
 c18aa7ab25ce7ffc566ea05e0bdc0bc7    42871 contrib/binary-s390x/Packages.gz
 7cd80e41d755c3cf2d82c5a7db114c91    36764 contrib/binary-s390x/Packages.xz
 1384e5fe776f68c5937a329d093f4574       97 contrib/binary-s390x/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-all/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-all/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-all/Packages.xz
 b983f5ec0ed51034cc113f1805c67953       95 contrib/debian-installer/binary-all/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-amd64/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-amd64/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-amd64/Packages.xz
 13849fb6c24cf98a16021fd4bd62a7b1       97 contrib/debian-installer/binary-amd64/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-arm64/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-arm64/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-arm64/Packages.xz
 5768c227cffe85bf92cf352e887df0c5       97 contrib/debian-installer/binary-arm64/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-armel/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-armel/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-armel/Packages.xz
 6b2e7091b1d0c922ad5a523baccace57       97 contrib/debian-installer/binary-armel/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-armhf/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-armhf/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-armhf/Packages.xz
 d8e7323ed580417ea00761ea0967d21a       97 contrib/debian-installer/binary-armhf/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-i386/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-i386/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-i386/Packages.xz
 dffa6d06c6a8f50d752b5458d2caa599       96 contrib/debian-installer/binary-i386/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-mips/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-mips/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-mips/Packages.xz
 c63d261437c8e9eb323c8fffe318b4b6       96 contrib/debian-installer/binary-mips/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-mipsel/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-mipsel/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-mipsel/Packages.xz
 fc6dd5bf12e345a87c5ad0a59b58e247       98 contrib/debian-installer/binary-mipsel/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-powerpc/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-powerpc/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-powerpc/Packages.xz
 c99cf261763db1fc1739292ae2215d2c       99 contrib/debian-installer/binary-powerpc/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-ppc64el/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-ppc64el/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-ppc64el/Packages.xz
 55c611ed63bc4c23810487378a788e20       99 contrib/debian-installer/binary-ppc64el/Release
 d41d8cd98f00b204e9800998ecf8427e        0 contrib/debian-installer/binary-s390x/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 contrib/debian-installer/binary-s390x/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 contrib/debian-installer/binary-s390x/Packages.xz
 1384e5fe776f68c5937a329d093f4574       97 contrib/debian-installer/binary-s390x/Release
 66001917ae5ab2fd87b4b1af9eec97b4   144523 contrib/i18n/Translation-en
 eed93cf51824ebdad7cf5d17cd57dd31    38528 contrib/i18n/Translation-en.bz2
 fea50b3e9c9afee38e8e5bd4b35e9616       98 contrib/source/Release
 5c10a800f309590a33dfa499fbe0128b   191039 contrib/source/Sources
 0d279dc0b67dade480d3e09dd02d0971    59528 contrib/source/Sources.gz
 1903c15f49240063ad2922251a11f8cc    50796 contrib/source/Sources.xz
 2e825f43a78dffaf7da511d93986f386 388115371 main/Contents-amd64
 b41921f64f118692ff9fce8597ef253d 27347433 main/Contents-amd64.gz
 0c5e6b947e04c16fd18fd4e09af12414 377276368 main/Contents-arm64
 436b4f80e86c0d1e8a12e431d4d83ad2 26457779 main/Contents-arm64.gz
 74f6b466c865207b4b71ba0a9494ed79 384735136 main/Contents-armel
 f056b35dcd50136577f38be899cdef5f 27017480 main/Contents-armel.gz
 3e45883d12d6171a558510fb68738870 384395929 main/Contents-armhf
 3761b07d559b0a531fc71def15e45ea2 27006099 main/Contents-armhf.gz
 f3b5a7ec040e806241741ec9615b1175 389537589 main/Contents-i386
 e43e1e424aef5dcd6bee4bff111273b6 27459469 main/Contents-i386.gz
 87f16d81e8f7bb19b3fe38a05628b633 383659523 main/Contents-mips
 50e34122b3d0ecc73f9e0593f9c8f242 26925567 main/Contents-mips.gz
 11b014f041fc30d81727dbce750c8ffa 384238777 main/Contents-mipsel
 8e6b0ce629026dff3fe7c5177ba550d9 26954916 main/Contents-mipsel.gz
 4203c7d18edbd06fa9c15cd12da1f2cc 385718105 main/Contents-powerpc
 cd65041c8f27a5d4fed93ccdbe89eabd 27121913 main/Contents-powerpc.gz
 bdf21d0e593d535ea4adf29eb46fdfaa 378194384 main/Contents-ppc64el
 8a085445be86225557c96b00483a055d 26520818 main/Contents-ppc64el.gz
 aa81ff764ebd6b59d5f37448669e9791 380255909 main/Contents-s390x
 36321db6a99f9148a769eb85447144c6 26734701 main/Contents-s390x.gz
 70635b05c6572260793bf5473f0892b7 373524727 main/Contents-source
 631e47207301cd0fd374d3a79ed5a4ae 43001444 main/Contents-source.gz
 af9f0221b2fe542ca38441b97731038a   349950 main/Contents-udeb-amd64
 f6bf9835a7b3d10da77ab23615e87b7a    29020 main/Contents-udeb-amd64.gz
 b6cccc82c023beebf10f6afe5485ccbe   292065 main/Contents-udeb-arm64
 02192ac567a3d108dcb49dcbdb4129f9    25263 main/Contents-udeb-arm64.gz
 301949130e367783307be20451d82d8f   333976 main/Contents-udeb-armel
 27a04c216447e617b10ee457b26219d2    27246 main/Contents-udeb-armel.gz
 5df643349ceaa4a6699d3d251100ed35   335334 main/Contents-udeb-armhf
 d615579d2fe10c2f530770ef2cba404d    28397 main/Contents-udeb-armhf.gz
 44e90031d089ed50e4940a9975c908d3   449242 main/Contents-udeb-i386
 420060beb47e6460d689e15f776e826e    35628 main/Contents-udeb-i386.gz
 74ded650ee0f21327019d6d901c4be1a   459596 main/Contents-udeb-mips
 30d62c7d310ec97429034faeb91249f1    35461 main/Contents-udeb-mips.gz
 8541c0f26c3d1193f7876e5f1775ffad   577556 main/Contents-udeb-mipsel
 3189ef63b88c54ff2fd708bc1ca1e611    42716 main/Contents-udeb-mipsel.gz
 32b5537384a1dfe8a9a80b62015260d4   415803 main/Contents-udeb-powerpc
 27a6e77205881d386c90dad3a6276935    32808 main/Contents-udeb-powerpc.gz
 c2d0dcaec96e5f83638b26fc16b4c199   310967 main/Contents-udeb-ppc64el
 5895544eb45c23353073e1998ff2eaf3    26330 main/Contents-udeb-ppc64el.gz
 c943359120aee60621086c8d7703d191   271052 main/Contents-udeb-s390x
 38b6e5c2a6d411a9b72a58e4c8f0cee3    23599 main/Contents-udeb-s390x.gz
 326996aa73b63358c2b7083df5c8f537 14116035 main/binary-all/Packages
 7bbe0256cc1b203890c04055d58e0ffd  3927273 main/binary-all/Packages.gz
 664ece2ceb2850970a2070770566f5bf  2996384 main/binary-all/Packages.xz
 932b7352c9a4d506af2563625598ba88       92 main/binary-all/Release
 f3343cb1981a25cecf6a3392b76052b8 33899150 main/binary-amd64/Packages
 885a823fc6e3e1261ed91d03d21713e5  9049024 main/binary-amd64/Packages.gz
 a65675c070735d33cad548119252001a  6776408 main/binary-amd64/Packages.xz
 aadd67182981800c83d66332631cc80f       94 main/binary-amd64/Release
 59f06fee2e46a8346722fa490b4a22ec 31925593 main/binary-arm64/Packages
 c3b05ff8b6d44ba4adef96c9dfabaac8  8543977 main/binary-arm64/Packages.gz
 ace2a0eb5c3e5fcdea1b48d55b6544b3  6405324 main/binary-arm64/Packages.xz
 c452a4d335bf3991dba1fb68af90cb41       94 main/binary-arm64/Release
 7e60cd118d655992950ab601cc0b521e 33114041 main/binary-armel/Packages
 941a16bb174cfa46d14eb32829e551f5  8852522 main/binary-armel/Packages.gz
 d00fba876becea3a9c907b80b7319533  6632496 main/binary-armel/Packages.xz
 a2986d1e394e619840c189c30b3e5cdf       94 main/binary-armel/Release
 2a7bb24277199181187db82f1331f644 33104737 main/binary-armhf/Packages
 d01953c3794d3d867651470e0a7dac55  8849561 main/binary-armhf/Packages.gz
 5b71ccf6e49d3b81746db55b47b30b49  6631896 main/binary-armhf/Packages.xz
 d62c7ab8f5f626ddab3401c2bf8a0e73       94 main/binary-armhf/Release
 6b0f2a88e69b6a3fe762ec2ea18ad1e7 33870550 main/binary-i386/Packages
 ea7bee2b885d01b8b75c2403ed87c433  9051240 main/binary-i386/Packages.gz
 bda3fef42b45c1ecf679166abfc23630  6779128 main/binary-i386/Packages.xz
 2bd2a5c8f607acf7acc25a75ea2cf5fc       93 main/binary-i386/Release
 e98bd4edd368ec108adddaeaa89d18a9 32797474 main/binary-mips/Packages
 f1d3ca82991a6bc60a07a06000c3ca94  8792469 main/binary-mips/Packages.gz
 e2551a474bdda830cc7bd02fec08f2f4  6585720 main/binary-mips/Packages.xz
 554ddacc61de6f7d49c2ae5d1b207efb       93 main/binary-mips/Release
 da463198360da83dba9e714a49307bc6 32955178 main/binary-mipsel/Packages
 846a255c2753b70ea5e5632977c52a56  8817773 main/binary-mipsel/Packages.gz
 0c537a0a082af66c0b1d3da6be513de5  6603996 main/binary-mipsel/Packages.xz
 c2506de28cead851c5572359cf515e38       95 main/binary-mipsel/Release
 225623102e8f621d6e8f10fca1840340 33428595 main/binary-powerpc/Packages
 939df37a9a125d38a9c074cefbcb8ef1  8913905 main/binary-powerpc/Packages.gz
 931b159674f8411b989e50017f9c5a6e  6674352 main/binary-powerpc/Packages.xz
 de94460b6e5dbccad943f056e8afdb68       96 main/binary-powerpc/Release
 c014b0edb3dec8a2e6a1681851ddcffd 32449606 main/binary-ppc64el/Packages
 b53c335f6aa0f5277474887fadbb123d  8657717 main/binary-ppc64el/Packages.gz
 4ac54a0d25e09ed6787e1ebf6c443b46  6487204 main/binary-ppc64el/Packages.xz
 66c01f6aa188b51e5ebf3318cf374498       96 main/binary-ppc64el/Release
 c0caec67f796d5c5614bb6d35ecc2c69 32646123 main/binary-s390x/Packages
 99a318f0062ce925f5b6adb013ecb880  8747482 main/binary-s390x/Packages.gz
 578b434f3bd20e925d1d1b19e15b2b39  6550600 main/binary-s390x/Packages.xz
 5c97b5013092cbd22425bbbe82eff5be       94 main/binary-s390x/Release
 2f9e297b277b3ebef1e97de48dca8880    67861 main/debian-installer/binary-all/Packages
 98c256873611429b2e9b8085b7d83d45    19852 main/debian-installer/binary-all/Packages.gz
 f17cd013b53f9e784b2b403c59d3170f    17168 main/debian-installer/binary-all/Packages.xz
 932b7352c9a4d506af2563625598ba88       92 main/debian-installer/binary-all/Release
 f5380f01fcb73c8b0266e56d94552862   238176 main/debian-installer/binary-amd64/Packages
 6a16c26e9d15666007ea4c60bd68aa14    68677 main/debian-installer/binary-amd64/Packages.gz
 557ad5c8d401242f38921ede189f960b    57148 main/debian-installer/binary-amd64/Packages.xz
 aadd67182981800c83d66332631cc80f       94 main/debian-installer/binary-amd64/Release
 fe6af5b396d97d3b654f0a2220aa8445   221242 main/debian-installer/binary-arm64/Packages
 73eb105a5ee5be0f734f584435d7bf90    63951 main/debian-installer/binary-arm64/Packages.gz
 e6b180b07a6227f19748e4f0c2ba8af9    53708 main/debian-installer/binary-arm64/Packages.xz
 c452a4d335bf3991dba1fb68af90cb41       94 main/debian-installer/binary-arm64/Release
 60932b17d12b0b936647fff2f0a897f5   264220 main/debian-installer/binary-armel/Packages
 b67d92e6254535385ad6871006decc81    72013 main/debian-installer/binary-armel/Packages.gz
 b07b30d39d8fedec3fab5d81f2c90598    60384 main/debian-installer/binary-armel/Packages.xz
 a2986d1e394e619840c189c30b3e5cdf       94 main/debian-installer/binary-armel/Release
 61e48bbe459bd8f7e79c719d59b145ad   223152 main/debian-installer/binary-armhf/Packages
 84999500bf4e765d330cba795a80e8bb    65038 main/debian-installer/binary-armhf/Packages.gz
 629f7ddaca8ea37dde93827f999b5a5f    54336 main/debian-installer/binary-armhf/Packages.xz
 d62c7ab8f5f626ddab3401c2bf8a0e73       94 main/debian-installer/binary-armhf/Release
 53be9656f6668b65fa4730ea426aae19   276184 main/debian-installer/binary-i386/Packages
 54acf12e1305f842a5bb29af8a0af319    75214 main/debian-installer/binary-i386/Packages.gz
 1189d2b38cff073b1b0fa9372bcaafc5    62864 main/debian-installer/binary-i386/Packages.xz
 2bd2a5c8f607acf7acc25a75ea2cf5fc       93 main/debian-installer/binary-i386/Release
 db0d061beff0cbebff8be399aa3312e5   311838 main/debian-installer/binary-mips/Packages
 2edf940f5f61249ca0579458151a2946    80395 main/debian-installer/binary-mips/Packages.gz
 d8de7a328e415f07b74e7c6e3b54ad1a    67344 main/debian-installer/binary-mips/Packages.xz
 554ddacc61de6f7d49c2ae5d1b207efb       93 main/debian-installer/binary-mips/Release
 8908fe4aa2616abbb1948f9785c2efb9   355242 main/debian-installer/binary-mipsel/Packages
 d112d221cd62301780a5fef4d2edf342    86928 main/debian-installer/binary-mipsel/Packages.gz
 356a59b12f25a3ec3fe279e335810f5f    72992 main/debian-installer/binary-mipsel/Packages.xz
 c2506de28cead851c5572359cf515e38       95 main/debian-installer/binary-mipsel/Release
 d6b204c1838924c16d8061ffd675efc0   268225 main/debian-installer/binary-powerpc/Packages
 1a8771fd820ba396c8c7e82935843e4d    72930 main/debian-installer/binary-powerpc/Packages.gz
 d7671f467ce1ba91c09f5c663ac3a325    61236 main/debian-installer/binary-powerpc/Packages.xz
 de94460b6e5dbccad943f056e8afdb68       96 main/debian-installer/binary-powerpc/Release
 0fdd5934cb19b45422d7653de7cc39ee   225934 main/debian-installer/binary-ppc64el/Packages
 dc7cc5cbae7129bc412ee18aff031a71    64315 main/debian-installer/binary-ppc64el/Packages.gz
 d4d11d14e76ca903269c25393c0cbfaf    54260 main/debian-installer/binary-ppc64el/Packages.xz
 66c01f6aa188b51e5ebf3318cf374498       96 main/debian-installer/binary-ppc64el/Release
 9b758db77d21b124370a5bb3aa0fbdfe   206467 main/debian-installer/binary-s390x/Packages
 a5f2fa0d717472a1c7f97ce29982c763    60857 main/debian-installer/binary-s390x/Packages.gz
 3b669d4194813608783ee419e3801371    51020 main/debian-installer/binary-s390x/Packages.xz
 5c97b5013092cbd22425bbbe82eff5be       94 main/debian-installer/binary-s390x/Release
 348635c0ff26a9daf03afc05e2ae9f8d     9627 main/i18n/Translation-ca
 7769b51bb58472bb34d711bf635fa4e2     3865 main/i18n/Translation-ca.bz2
 0927d045de9ea149e8b1fe08daf2a4ff  1893411 main/i18n/Translation-cs
 dd719c7148e2327b641271f0f3c680d4   494559 main/i18n/Translation-cs.bz2
 8446964ffff316782c4a018e41bab84c 13166981 main/i18n/Translation-da
 429907fbcb864d45f5a2274f5a967053  2901281 main/i18n/Translation-da.bz2
 d11cd1c53d0a3540e211f3f934cc874a  7664182 main/i18n/Translation-de
 d5eb8cfc7a184d1bd8bafc528587367d  1754517 main/i18n/Translation-de.bz2
 a344219bf0eec9139d5270017ecfceee     1347 main/i18n/Translation-de_DE
 0fe0725f74bb5249f15f30ce965142d5      830 main/i18n/Translation-de_DE.bz2
 c26d1f5d454d59a734435115a463bc5e     8287 main/i18n/Translation-el
 a2e3d718b49885932660c9c5dbdc0570     1896 main/i18n/Translation-el.bz2
 15989280f2195e658e4c0f95cab9f143 22326673 main/i18n/Translation-en
 e0f862a435644ae2f050cb14d97e293e  4582093 main/i18n/Translation-en.bz2
 0c8ebf866e74ebf2d13a6667b2de8898     2739 main/i18n/Translation-eo
 20eada7675a89d6de9ef9f9d5bb50c9b     1344 main/i18n/Translation-eo.bz2
 7b4d0aa1325b4152ce07cec6b4d425cf  1278186 main/i18n/Translation-es
 4c61b5e2f94bf4204d9c4a869aef68ff   314368 main/i18n/Translation-es.bz2
 bd07e221727c32816e495848e7cd0f5b    17288 main/i18n/Translation-eu
 18992bed6a6b01a12333635b1d94773d     6460 main/i18n/Translation-eu.bz2
 69fe85558f11c0b1559101dcde685542   390801 main/i18n/Translation-fi
 81dbe038b66ed6c0854c28c0631510a2   108973 main/i18n/Translation-fi.bz2
 c25501d80385928ee7e3f06916258c96  3701781 main/i18n/Translation-fr
 c024066a0c48fd9b7506954934bc9adf   846329 main/i18n/Translation-fr.bz2
 32d079a7a5251723d01e7d44a501a68a    14962 main/i18n/Translation-hr
 81cdaae6c7ecfed80125c6eb75efe310     5841 main/i18n/Translation-hr.bz2
 d4d76a6110bac15c7a69ff809219460b   101201 main/i18n/Translation-hu
 b21515add52faa84eda9e044f2af7d97    33209 main/i18n/Translation-hu.bz2
 34ec59df8bca0339a18f33e475ec9965     7753 main/i18n/Translation-id
 c1adb75e39ecbdc6eadd353d22b47f7d     3093 main/i18n/Translation-id.bz2
 df4e81c737205744679ae0c8a3f72027 17993956 main/i18n/Translation-it
 2b9a51f43f55e899ab0b6230ef5086a9  3656485 main/i18n/Translation-it.bz2
 69582fcad0506a2b759b325ae5a25b70  4376703 main/i18n/Translation-ja
 e526f2a3797ecea54169187837f53202   794698 main/i18n/Translation-ja.bz2
 b63e768393ddb6060fab105db3c39605    21141 main/i18n/Translation-km
 382da97227f82fb8baf5e7afb121787a     3796 main/i18n/Translation-km.bz2
 20bae7cbe48fd0a36127b4541dcdca73   924206 main/i18n/Translation-ko
 01651e0f40bdf6c3da8e0258152d5b2a   204449 main/i18n/Translation-ko.bz2
 d41d8cd98f00b204e9800998ecf8427e        0 main/i18n/Translation-ml
 4059d198768f9f8dc9372dc1c54bc3c3       14 main/i18n/Translation-ml.bz2
 5676b1dbf1afed1dfd2ed8c4d87d6f8e     2325 main/i18n/Translation-nb
 be1e645de35839db0778d87b8e903e43     1302 main/i18n/Translation-nb.bz2
 34c113f41d99028bdb0709d2aa36479c   267344 main/i18n/Translation-nl
 4223513a6e2e1872c62dfad54e8dd5ca    74095 main/i18n/Translation-nl.bz2
 5cfb340cfad97a0a262b4c5ac90fe12c  2490134 main/i18n/Translation-pl
 8ec7270b392b184961302dd86332c363   574404 main/i18n/Translation-pl.bz2
 d1b03ae2be31e5853195c11d0d0310fb  1712133 main/i18n/Translation-pt
 ff59118e6b9b99b5d8f361d9bbc3e3e5   413142 main/i18n/Translation-pt.bz2
 5c86875535d6d5c8a7ffcede865ee530  3363311 main/i18n/Translation-pt_BR
 983c9e3d823c946e64c542d4f9243684   802754 main/i18n/Translation-pt_BR.bz2
 9574eddfdb6ee4fa59d2cf7e74a63aa3     2949 main/i18n/Translation-ro
 82c63f5ea9cfd275bea9b476f34b6653     1463 main/i18n/Translation-ro.bz2
 211ea3dd132616c2d8dd9d1789225fd8  2684275 main/i18n/Translation-ru
 ef6476298fc0a11e5ebbc5cd97d764de   437612 main/i18n/Translation-ru.bz2
 895452b9a3e593d4c414eb990aac7840  2106842 main/i18n/Translation-sk
 0d90fda69c024b6fc77a399284e4346a   509104 main/i18n/Translation-sk.bz2
 1ac5fae7259a2d9005931e9d067347c2   455684 main/i18n/Translation-sr
 0ecf73379f6a96cfa577007a358ca92a    81894 main/i18n/Translation-sr.bz2
 66a4d41cf016d506d4d49e4fbe9787f9   132973 main/i18n/Translation-sv
 7d33a2add5c76e5b5fb5ba576df43cf9    40142 main/i18n/Translation-sv.bz2
 617fcb26803e9b2a60c2716ea4744451      902 main/i18n/Translation-tr
 96c9deb10e4bb691e778589dec582fd7      530 main/i18n/Translation-tr.bz2
 b80c405f1ef48129f664b9ccb22beb1a  4630335 main/i18n/Translation-uk
 c7a6ff80d5640d5bed7f2af379cd2f03   734132 main/i18n/Translation-uk.bz2
 c67b37189067f0f61cac32ed56400701    36422 main/i18n/Translation-vi
 9b85ff05670f25504a9e4baeaeccfb59    10243 main/i18n/Translation-vi.bz2
 7717fa25bd691ec385bddacf42419c9f     2799 main/i18n/Translation-zh
 b5a4e7e47da47ac7596ed5d72fdeef93     1526 main/i18n/Translation-zh.bz2
 dc0784e7a66476e4816a019c6bd782e3   367410 main/i18n/Translation-zh_CN
 1953e9197757a528dedc8dd068d1bf9d   100783 main/i18n/Translation-zh_CN.bz2
 c891d0ca7a79055ce8aeeba053751805    64019 main/i18n/Translation-zh_TW
 666bbc4ff38def9af5bb1d06331f2b86    22485 main/i18n/Translation-zh_TW.bz2
 f7138b4460c5b748866bb8ef16a22a04    53815 main/installer-amd64/20150422+deb8u4+b2/images/MD5SUMS
 9ecaf703beaa8d2a5aff44c581162a9b    72131 main/installer-amd64/20150422+deb8u4+b2/images/SHA256SUMS
 c06312d27a05b395a5a45ae7c2507ab5    53815 main/installer-amd64/20150422/images/MD5SUMS
 bc3feb487bafcbdfd06e06284000e368    72131 main/installer-amd64/20150422/images/SHA256SUMS
 f7138b4460c5b748866bb8ef16a22a04    53815 main/installer-amd64/current/images/MD5SUMS
 9ecaf703beaa8d2a5aff44c581162a9b    72131 main/installer-amd64/current/images/SHA256SUMS
 3a719f21a07619874ea4804280613de9    19148 main/installer-arm64/20150422+deb8u4+b2/images/MD5SUMS
 277e07dc209d7de3df748f35625d48ac    25912 main/installer-arm64/20150422+deb8u4+b2/images/SHA256SUMS
 c52390d4695c984028c98a32f7a76dff    19148 main/installer-arm64/20150422/images/MD5SUMS
 f4160f9f0b4b49d8997ee2c920004594    25912 main/installer-arm64/20150422/images/SHA256SUMS
 3a719f21a07619874ea4804280613de9    19148 main/installer-arm64/current/images/MD5SUMS
 277e07dc209d7de3df748f35625d48ac    25912 main/installer-arm64/current/images/SHA256SUMS
 06258cb0a60db4fc9d454cfca7593375    11608 main/installer-armel/20150422+deb8u4+b2/images/MD5SUMS
 aefa8038dae474e67616bce06f33c7ba    16324 main/installer-armel/20150422+deb8u4+b2/images/SHA256SUMS
 42fdeb26bb11aeba2b73bb665394ed5d     8985 main/installer-armel/20150422/images/MD5SUMS
 5508c992d754756a76d94459694d0846    12645 main/installer-armel/20150422/images/SHA256SUMS
 06258cb0a60db4fc9d454cfca7593375    11608 main/installer-armel/current/images/MD5SUMS
 aefa8038dae474e67616bce06f33c7ba    16324 main/installer-armel/current/images/SHA256SUMS
 1c807bf4ad1562a05999328841cc487c    19599 main/installer-armhf/20150422+deb8u4+b2/images/MD5SUMS
 b84be71803100382f878f99e46d14588    28379 main/installer-armhf/20150422+deb8u4+b2/images/SHA256SUMS
 77d9da266c887d99f50e4a78770da16a    19599 main/installer-armhf/20150422/images/MD5SUMS
 e1ea97c8e0f0ab02e0acf66302b64072    28379 main/installer-armhf/20150422/images/SHA256SUMS
 1c807bf4ad1562a05999328841cc487c    19599 main/installer-armhf/current/images/MD5SUMS
 b84be71803100382f878f99e46d14588    28379 main/installer-armhf/current/images/SHA256SUMS
 ac62fad8b1e561e0f1aff4b8614ee839    52495 main/installer-i386/20150422+deb8u4+b2/images/MD5SUMS
 5d117d934605b8dd87916c2d96cbf055    70875 main/installer-i386/20150422+deb8u4+b2/images/SHA256SUMS
 6945a44a20596ed6e81e3dea9c2f3ddf    52495 main/installer-i386/20150422/images/MD5SUMS
 34ce5cc3c7b4ac330eb592aa8fb8c41e    70875 main/installer-i386/20150422/images/SHA256SUMS
 ac62fad8b1e561e0f1aff4b8614ee839    52495 main/installer-i386/current/images/MD5SUMS
 5d117d934605b8dd87916c2d96cbf055    70875 main/installer-i386/current/images/SHA256SUMS
 02e71d3b9cbe48f887bb083a2a36b71e      940 main/installer-mips/20150422+deb8u4+b2/images/MD5SUMS
 e08e36bc26db8631bed409523608097a     1496 main/installer-mips/20150422+deb8u4+b2/images/SHA256SUMS
 48639d5b34148741df2132cb9079bd79      940 main/installer-mips/20150422/images/MD5SUMS
 e592333fd6d054e559d15027b9c3ebaa     1496 main/installer-mips/20150422/images/SHA256SUMS
 02e71d3b9cbe48f887bb083a2a36b71e      940 main/installer-mips/current/images/MD5SUMS
 e08e36bc26db8631bed409523608097a     1496 main/installer-mips/current/images/SHA256SUMS
 54f4a0c85f30a25dc3e01ee8643d1626     1213 main/installer-mipsel/20150422+deb8u4+b2/images/MD5SUMS
 760c6a2096e05a3c797f94b4ce324d33     1865 main/installer-mipsel/20150422+deb8u4+b2/images/SHA256SUMS
 a1c3f76716993801388b8224d75b0968     1213 main/installer-mipsel/20150422/images/MD5SUMS
 266b1b098b295678ad04fba1e9734dd1     1865 main/installer-mipsel/20150422/images/SHA256SUMS
 54f4a0c85f30a25dc3e01ee8643d1626     1213 main/installer-mipsel/current/images/MD5SUMS
 760c6a2096e05a3c797f94b4ce324d33     1865 main/installer-mipsel/current/images/SHA256SUMS
 4db7511e2026ee09c1bcc44d9e8f1e95     2128 main/installer-powerpc/20150422+deb8u4+b2/images/MD5SUMS
 9240a564258100ee09753b0ac0e98ee8     3292 main/installer-powerpc/20150422+deb8u4+b2/images/SHA256SUMS
 757a78a03000d563e55139d067fd3463     2128 main/installer-powerpc/20150422/images/MD5SUMS
 76c2a153bd1076a232bf969c35155903     3292 main/installer-powerpc/20150422/images/SHA256SUMS
 4db7511e2026ee09c1bcc44d9e8f1e95     2128 main/installer-powerpc/current/images/MD5SUMS
 9240a564258100ee09753b0ac0e98ee8     3292 main/installer-powerpc/current/images/SHA256SUMS
 e15909b473a2fcc89e57bf2ae745f151      576 main/installer-ppc64el/20150422+deb8u4+b2/images/MD5SUMS
 423798620ba53526cba49f11dab43386      972 main/installer-ppc64el/20150422+deb8u4+b2/images/SHA256SUMS
 6730181232434b5e0b07f070e4037e5b      576 main/installer-ppc64el/20150422/images/MD5SUMS
 6b13d9e4427cf78bdd9a3b70ac84ff0b      972 main/installer-ppc64el/20150422/images/SHA256SUMS
 e15909b473a2fcc89e57bf2ae745f151      576 main/installer-ppc64el/current/images/MD5SUMS
 423798620ba53526cba49f11dab43386      972 main/installer-ppc64el/current/images/SHA256SUMS
 34e474b7b81ae3f0dc3d409bccdf077a      374 main/installer-s390x/20150422+deb8u4+b2/images/MD5SUMS
 7205e274e22afaf8ef77da25cbf57474      674 main/installer-s390x/20150422+deb8u4+b2/images/SHA256SUMS
 9a80df21f5bd9c52f25e424e490f97bb      374 main/installer-s390x/20150422/images/MD5SUMS
 20355389634104504def0522c0a4f62c      674 main/installer-s390x/20150422/images/SHA256SUMS
 34e474b7b81ae3f0dc3d409bccdf077a      374 main/installer-s390x/current/images/MD5SUMS
 7205e274e22afaf8ef77da25cbf57474      674 main/installer-s390x/current/images/SHA256SUMS
 d681fed105652b6ae53fc8898663abb0       95 main/source/Release
 03514f1949e257b5995f7fce5d22cfae 32518618 main/source/Sources
 2bf1f34e71499bbcca2062dad6d8d52d  9156980 main/source/Sources.gz
 9571f826cd78ac8b4d83b908a0fdeb2f  7055888 main/source/Sources.xz
 77a17e2deb4323f98562c2e81c642fb3 13875760 non-free/Contents-amd64
 0bd15ecf852f9181239f05ccd5eed881   780181 non-free/Contents-amd64.gz
 e696940a1a83b89b38820e0519e146b9 12901760 non-free/Contents-arm64
 887ec7e20e874f4619e6c6f6d3e87a27   702742 non-free/Contents-arm64.gz
 a23bdde1e4202a7ce37d1855e9cf1990 12974290 non-free/Contents-armel
 44d2c4459abb2bb3501b6b976ad6f1b9   709306 non-free/Contents-armel.gz
 8527437f6b5a7cf978b9f61672d18073 12999757 non-free/Contents-armhf
 23e568bc043948dcb00171e52228e2dd   711618 non-free/Contents-armhf.gz
 392a1e526839e0b3ec29827b6f60381d 13788194 non-free/Contents-i386
 b5c6fc8ba63360bcb7d7a6ba2bbb3044   773196 non-free/Contents-i386.gz
 e04a8d3259e805fd153d5a02c2eba68d 12988374 non-free/Contents-mips
 92c6b8d1e52daee2040c4509999c792f   711304 non-free/Contents-mips.gz
 70559130734ef0661f28eab8be18f696 12993364 non-free/Contents-mipsel
 d2bea90dfbc602bc042b4522205f7756   711343 non-free/Contents-mipsel.gz
 63221668665bfbc93ab5415b3cee4cc6 12984126 non-free/Contents-powerpc
 11c731bf637cf0d1430064e0f941d2fd   710607 non-free/Contents-powerpc.gz
 95f20dfac0a6dd95e52e39dc38ca17fc 12948073 non-free/Contents-ppc64el
 cf0fbb7010d6fe84c622bb760f5d39a2   707183 non-free/Contents-ppc64el.gz
 1e0544c855a2b81eadd0fc85de9a7c6a 12985547 non-free/Contents-s390x
 a93efc48db46f8b7740e3819dbc00f2d   710715 non-free/Contents-s390x.gz
 8e497ffbb29244252b1700664dbe0ba9  8565950 non-free/Contents-source
 aa913049ae52d25784784cc0b91d97fd   917043 non-free/Contents-source.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-amd64
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-amd64.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-arm64
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-arm64.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-armel
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-armel.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-armhf
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-armhf.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-i386
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-i386.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-mips
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-mips.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-mipsel
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-mipsel.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-powerpc
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-powerpc.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-ppc64el
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-ppc64el.gz
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/Contents-udeb-s390x
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/Contents-udeb-s390x.gz
 9cf895c004430a510d5b75e9ef1f41db   190308 non-free/binary-all/Packages
 15a84df80b081985f6859b1d4cc6842c    56603 non-free/binary-all/Packages.gz
 54b59f4744edc165ba23ea05c23d3919    47968 non-free/binary-all/Packages.xz
 db9dc2331a719e3500a83c26f68d669d       96 non-free/binary-all/Release
 c5c6a5113df9335e0a8dfe33c6a61347   355467 non-free/binary-amd64/Packages
 cd07af8b771df22c7adbdca0397eee6b   101206 non-free/binary-amd64/Packages.gz
 a80bae9d09797d737e60c7344d130bf5    83648 non-free/binary-amd64/Packages.xz
 7e86bc13bdc32ffdbe496ea3a46aad43       98 non-free/binary-amd64/Release
 745d3372355fa0193c0fe7142714c3df   230493 non-free/binary-arm64/Packages
 ebe735eb674853ea453f05428f403617    68723 non-free/binary-arm64/Packages.gz
 d7857c63a5f3c762e0367eff2f6c5601    57432 non-free/binary-arm64/Packages.xz
 b3421724b0b5ee291a25219fa2fff095       98 non-free/binary-arm64/Release
 fe510c06ebd90a03ca7e5c8b61e20607   239168 non-free/binary-armel/Packages
 04d19eda6aa60e225cc9e5c83f0ca492    71096 non-free/binary-armel/Packages.gz
 325f519fc0e3cde213719b765512c8df    59656 non-free/binary-armel/Packages.xz
 71d306ea9c10e2833886a84c3c6be09c       98 non-free/binary-armel/Release
 612d5d7c48232fca31c334acb153ea22   255719 non-free/binary-armhf/Packages
 94a95288044b2664c7159dcb749b8e8a    74897 non-free/binary-armhf/Packages.gz
 8ff625e48fcd5762ae05140c3c43c462    62484 non-free/binary-armhf/Packages.xz
 2b9ee50ee6609a6b4a2c23c36fd7c902       98 non-free/binary-armhf/Release
 328e79dd00f8eb7c5e14410a6aec63cf   344437 non-free/binary-i386/Packages
 91b124e4f18bc7179620affc43e824fb    96963 non-free/binary-i386/Packages.gz
 4d7261a19f77263fd583b946ebbb2193    80432 non-free/binary-i386/Packages.xz
 acf2617ab3a16e0f5790f74dbe396599       97 non-free/binary-i386/Release
 718191cf83831f5f9a846815d338bfea   240596 non-free/binary-mips/Packages
 f8f12e5cb80e53a72f66a64b42c4153c    71671 non-free/binary-mips/Packages.gz
 eec2096b47d4d9846ded2f0e538f133b    59864 non-free/binary-mips/Packages.xz
 fbc1b41b6dbb0925059e361207b2687e       97 non-free/binary-mips/Release
 c2d6e10faf92a61571a5e6018a9a12f8   243099 non-free/binary-mipsel/Packages
 001646eace995572823ad38956538bad    72082 non-free/binary-mipsel/Packages.gz
 698e99a6c54a363855134f403a4c92e6    60296 non-free/binary-mipsel/Packages.xz
 7c93761c32f9570983c957f028d74b6b       99 non-free/binary-mipsel/Release
 8206bfd71bfe092aa5e1e75a14ebb5bb   239066 non-free/binary-powerpc/Packages
 eb187e7fb45257dd35b127f4d6a69d48    71145 non-free/binary-powerpc/Packages.gz
 4d46de7831b9deec932f14a55771cdf4    59496 non-free/binary-powerpc/Packages.xz
 a055b2507e7191b8c036a1cea1111656      100 non-free/binary-powerpc/Release
 f01c0e5877173074d79b92d090d5a2d4   233710 non-free/binary-ppc64el/Packages
 d2060d5943c6bd624376cbbd86b39cd1    69140 non-free/binary-ppc64el/Packages.gz
 41bf18d9d9dbe734fc3c9cff9763db7a    58056 non-free/binary-ppc64el/Packages.xz
 3238f035c8b12de24c59aec0e690a4a2      100 non-free/binary-ppc64el/Release
 198f650e9822fdd4016747e03f27f26d   239446 non-free/binary-s390x/Packages
 f42ec431d71ac77df0d97b669ca4dc62    71176 non-free/binary-s390x/Packages.gz
 a9aecfb2a0e5d510d5b83fca774f2ad0    59472 non-free/binary-s390x/Packages.xz
 588aa393340eeff821a1280ac07d7d0a       98 non-free/binary-s390x/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-all/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-all/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-all/Packages.xz
 db9dc2331a719e3500a83c26f68d669d       96 non-free/debian-installer/binary-all/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-amd64/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-amd64/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-amd64/Packages.xz
 7e86bc13bdc32ffdbe496ea3a46aad43       98 non-free/debian-installer/binary-amd64/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-arm64/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-arm64/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-arm64/Packages.xz
 b3421724b0b5ee291a25219fa2fff095       98 non-free/debian-installer/binary-arm64/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-armel/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-armel/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-armel/Packages.xz
 71d306ea9c10e2833886a84c3c6be09c       98 non-free/debian-installer/binary-armel/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-armhf/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-armhf/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-armhf/Packages.xz
 2b9ee50ee6609a6b4a2c23c36fd7c902       98 non-free/debian-installer/binary-armhf/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-i386/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-i386/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-i386/Packages.xz
 acf2617ab3a16e0f5790f74dbe396599       97 non-free/debian-installer/binary-i386/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-mips/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-mips/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-mips/Packages.xz
 fbc1b41b6dbb0925059e361207b2687e       97 non-free/debian-installer/binary-mips/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-mipsel/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-mipsel/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-mipsel/Packages.xz
 7c93761c32f9570983c957f028d74b6b       99 non-free/debian-installer/binary-mipsel/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-powerpc/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-powerpc/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-powerpc/Packages.xz
 a055b2507e7191b8c036a1cea1111656      100 non-free/debian-installer/binary-powerpc/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-ppc64el/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-ppc64el/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-ppc64el/Packages.xz
 3238f035c8b12de24c59aec0e690a4a2      100 non-free/debian-installer/binary-ppc64el/Release
 d41d8cd98f00b204e9800998ecf8427e        0 non-free/debian-installer/binary-s390x/Packages
 4a4dd3598707603b3f76a2378a4504aa       20 non-free/debian-installer/binary-s390x/Packages.gz
 8dc5aea5b03dff8595f096f9e368e888       32 non-free/debian-installer/binary-s390x/Packages.xz
 588aa393340eeff821a1280ac07d7d0a       98 non-free/debian-installer/binary-s390x/Release
 0ee832b85b99c44b85810d870eb28e6f   309527 non-free/i18n/Translation-en
 2c45903464182cc4cf7e84d04c05c9c2    72067 non-free/i18n/Translation-en.bz2
 554ecff983936bb1990cd15f30461555       99 non-free/source/Release
 ec12ad39e4b84f5f981295ec458ebd3a   397177 non-free/source/Sources
 06d4dd0b7b9ed5a9ddd2ea4e6d3cf5eb   119076 non-free/source/Sources.gz
 e8475c72bf3457521fdfdf97a0ef803a    99496 non-free/source/Sources.xz
SHA1:
 2c1f679daca5e2c625820e56a3dc367fc097ab42  1194094 contrib/Contents-amd64
 856551a733e707a1c1505aa2c8f43e433c7e016c    88313 contrib/Contents-amd64.gz
 93e1037941bbfed5a9a186df952d220825519bcf  1021028 contrib/Contents-arm64
 8c660e6f44ddb4d3da273ae0a59b090692d0c660    72294 contrib/Contents-arm64.gz
 6a6eb20d86d4a4a5cbd8f40d5aa071a3359fbb5e  1035060 contrib/Contents-armel
 df9ad877908e196287e91c0b2beaafe9b8db2bf7    73507 contrib/Contents-armel.gz
 760540b37d4764ffd7eade141c83b14afad8f9fc  1027963 contrib/Contents-armhf
 2f55bcffa8206572b5d7efa49d364397732b8bc0    73418 contrib/Contents-armhf.gz
 4200d4155101d7d452fba4a56b01a430ca5aaad6  1190658 contrib/Contents-i386
 6410dbbe73b02bb89656d71573c5e03af2ac10d9    87973 contrib/Contents-i386.gz
 3ac70655f20bab93313789495f31659817cb821c  1036779 contrib/Contents-mips
 e62b26b2a9686a755d0458990ebfc5edb4362f42    73599 contrib/Contents-mips.gz
 8894bafe57b4078972af40bf65ef998565eed8f6  1036785 contrib/Contents-mipsel
 eb484f2806f11cd1674b7a03f9ee5d97e28ed56e    73704 contrib/Contents-mipsel.gz
 0fb99362365636f9c0d9311cec31a5fe0f1103be  1037497 contrib/Contents-powerpc
 938e6bc4a1cd9429c5d6c9bf018db0bd2f902987    73868 contrib/Contents-powerpc.gz
 eee3ae8df71cf6c0a8bd0b29ecfac0dce865cacb  1017446 contrib/Contents-ppc64el
 c89bee753be08cd55d836ba00b301086bf6155e2    71910 contrib/Contents-ppc64el.gz
 1a1d4aea5259e8f5968daf0b33d7d07858578531  1032989 contrib/Contents-s390x
 f93dcf699ac984d04832c7f83fd5e7eab4ff86f5    73259 contrib/Contents-s390x.gz
 7701e031ae2392e18a363aa9a6d0af9b285a2cf3  3015028 contrib/Contents-source
 ed3fd631be41a60e455e9eeea5f2230131b502e0   334510 contrib/Contents-source.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-amd64
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-amd64.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-arm64
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-arm64.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-armel
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-armel.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-armhf
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-armhf.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-i386
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-i386.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-mips
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-mips.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-mipsel
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-mipsel.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-powerpc
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-powerpc.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-ppc64el
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-ppc64el.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/Contents-udeb-s390x
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/Contents-udeb-s390x.gz
 7d31614920b3da0f0b23052917ae996845a8874a    84184 contrib/binary-all/Packages
 5cd50357b1a0f1fb66d578a805ad0260824aee3f    27124 contrib/binary-all/Packages.gz
 ed66f6ff571c480891571f73670a81681d6363bd    24000 contrib/binary-all/Packages.xz
 63388bc3bb1cbba060fde0a15e838e513291ef86       95 contrib/binary-all/Release
 3daf1e239d18c0f629015c1682737c8ef7f89bcf   197960 contrib/binary-amd64/Packages
 83b78237cbd6a26f4f0520e2a56585376d8e4772    59500 contrib/binary-amd64/Packages.gz
 59e4fdd7a85391156bc7a8274c826cd01003c27d    50168 contrib/binary-amd64/Packages.xz
 e1eda6752b21ef4178ad19a51e3f3a04f3784d81       97 contrib/binary-amd64/Release
 797daf71a5429446729d88b446962b4d6b221588   133508 contrib/binary-arm64/Packages
 a597a475fc95702eb93605d5befdf3fb6ca4e1c4    41575 contrib/binary-arm64/Packages.gz
 51b526fc5b3dc8eb3b789260ae164fe4d1aca969    35840 contrib/binary-arm64/Packages.xz
 9995476d4800c6451330653d0d9c4304eea8d2c6       97 contrib/binary-arm64/Release
 0375e19a8a2b32d90fbfbdc6ab69c085da3933b8   139204 contrib/binary-armel/Packages
 6b178c607028963658cf8af33b5e512e1715c580    43258 contrib/binary-armel/Packages.gz
 59e3493b3d824256ccf7af931a2c985170bfe2d8    37100 contrib/binary-armel/Packages.xz
 939c90c02d207d39592d629bab6a370bd436b667       97 contrib/binary-armel/Release
 c27d3944047d8cdf8b035e6a5ff7684ff1b3fe6d   144473 contrib/binary-armhf/Packages
 18f6271c5f469228f4d72af5c3a28efd10949fb2    44700 contrib/binary-armhf/Packages.gz
 2f82b6e75bc2632def1d24e75393d7a2dbc1a9f5    38136 contrib/binary-armhf/Packages.xz
 311c5c168783707150449ecb12d378e2920579a9       97 contrib/binary-armhf/Release
 519e5c22cc29a5493c0b76e575786ee0978d7460   194806 contrib/binary-i386/Packages
 1a407599b3ecd5d2e5452b523a96af267a6df611    58600 contrib/binary-i386/Packages.gz
 140be17dbc9e09d13aae0480b99de393b2f1a781    49492 contrib/binary-i386/Packages.xz
 2271aea38637b90d78e9a1b1698903c8c60d9392       96 contrib/binary-i386/Release
 8281bcfeb60c19c4090d6be57e66078f267227d2   140865 contrib/binary-mips/Packages
 256fda3260bdc982aac2555893bed12a07043adf    44000 contrib/binary-mips/Packages.gz
 a48a9a5324ba9d90e05c827e8e1817d64cb90e28    37540 contrib/binary-mips/Packages.xz
 2f290a8c3c599572bb2ba010104fe71ed57d2c2c       96 contrib/binary-mips/Release
 7898562c4ce605d8c9284d5d3453763176726a4a   141128 contrib/binary-mipsel/Packages
 53b1124564dedecca335642a338e3c083dde5a1e    43785 contrib/binary-mipsel/Packages.gz
 0cc2793f515cd6a664ebe769c7ea263741eebc95    37568 contrib/binary-mipsel/Packages.xz
 19dc7f0c8c4e11e34fad3327adcb6b5b4c93a921       98 contrib/binary-mipsel/Release
 c29677fc41bea84ba9841401d95ba415104e1f50   141881 contrib/binary-powerpc/Packages
 da8c3424063e5e96587be5201fca5c87f74726f5    43955 contrib/binary-powerpc/Packages.gz
 f950410cdd8b6b55666070ab86f2f730bafb8436    37700 contrib/binary-powerpc/Packages.xz
 03e297567aebbe6d07f11f5e28f466b708a36397       99 contrib/binary-powerpc/Release
 609b938931ed2da9b19ae1b535121c84a39b1aae   132753 contrib/binary-ppc64el/Packages
 671a80f030f20a6abbb0e4692bf1b715c0a4fadb    41486 contrib/binary-ppc64el/Packages.gz
 661c7c1604e25c0f4a69a9e5b7caadd9828390a3    35696 contrib/binary-ppc64el/Packages.xz
 c610aa6e06b66dc53589528e7c612500027885a5       99 contrib/binary-ppc64el/Release
 7639ed43937ae5b189acf5332632918baf5d95cc   137757 contrib/binary-s390x/Packages
 cf95394e2430970d07ddc4dd485097be016362b8    42871 contrib/binary-s390x/Packages.gz
 f1143aa7387f5a1fd051f79b7e912f094ba3b658    36764 contrib/binary-s390x/Packages.xz
 8f8890e2a33e5dd1bf261ca0de5b4c25a371279d       97 contrib/binary-s390x/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-all/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-all/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-all/Packages.xz
 63388bc3bb1cbba060fde0a15e838e513291ef86       95 contrib/debian-installer/binary-all/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-amd64/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-amd64/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-amd64/Packages.xz
 e1eda6752b21ef4178ad19a51e3f3a04f3784d81       97 contrib/debian-installer/binary-amd64/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-arm64/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-arm64/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-arm64/Packages.xz
 9995476d4800c6451330653d0d9c4304eea8d2c6       97 contrib/debian-installer/binary-arm64/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-armel/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-armel/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-armel/Packages.xz
 939c90c02d207d39592d629bab6a370bd436b667       97 contrib/debian-installer/binary-armel/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-armhf/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-armhf/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-armhf/Packages.xz
 311c5c168783707150449ecb12d378e2920579a9       97 contrib/debian-installer/binary-armhf/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-i386/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-i386/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-i386/Packages.xz
 2271aea38637b90d78e9a1b1698903c8c60d9392       96 contrib/debian-installer/binary-i386/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-mips/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-mips/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-mips/Packages.xz
 2f290a8c3c599572bb2ba010104fe71ed57d2c2c       96 contrib/debian-installer/binary-mips/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-mipsel/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-mipsel/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-mipsel/Packages.xz
 19dc7f0c8c4e11e34fad3327adcb6b5b4c93a921       98 contrib/debian-installer/binary-mipsel/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-powerpc/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-powerpc/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-powerpc/Packages.xz
 03e297567aebbe6d07f11f5e28f466b708a36397       99 contrib/debian-installer/binary-powerpc/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-ppc64el/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-ppc64el/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-ppc64el/Packages.xz
 c610aa6e06b66dc53589528e7c612500027885a5       99 contrib/debian-installer/binary-ppc64el/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 contrib/debian-installer/binary-s390x/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 contrib/debian-installer/binary-s390x/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 contrib/debian-installer/binary-s390x/Packages.xz
 8f8890e2a33e5dd1bf261ca0de5b4c25a371279d       97 contrib/debian-installer/binary-s390x/Release
 290e01f44ac7b31957711fbb6eb6c564abc98aaf   144523 contrib/i18n/Translation-en
 6d94b7fc44aba5369d46b12827b494ff9df8e7be    38528 contrib/i18n/Translation-en.bz2
 c4fe9ae3dcf1caab20d84d513cf3d0c1668d236c       98 contrib/source/Release
 e5ce78685d8026f308c82eb6eea22137491b90a0   191039 contrib/source/Sources
 633090d7618a96177caebd6cab293231b7e7aa02    59528 contrib/source/Sources.gz
 02475ee107bf65208127763ead404527aa769508    50796 contrib/source/Sources.xz
 6fd635f947c7a024b9502838b8324eebd232f2b5 388115371 main/Contents-amd64
 d3a371f96edacd7c7b25f871a00a4105f218ff8e 27347433 main/Contents-amd64.gz
 a27810cb6bea525f9af4a0efeaf054fca10bcfa5 377276368 main/Contents-arm64
 f784c69025eb2ac816132e7667e42f8aa9720b51 26457779 main/Contents-arm64.gz
 b46c868de70b23e3820f6b9bcfe3f2eacbe29ea6 384735136 main/Contents-armel
 88f042886ce1848cc65fe728aed6340bb5525b9f 27017480 main/Contents-armel.gz
 003100c177487291bc32754b8c00c749eff4a903 384395929 main/Contents-armhf
 41798f9ec086914024f197d96381b626b7972290 27006099 main/Contents-armhf.gz
 afbab95c53db3966b03a9b8f95eecb21417af10f 389537589 main/Contents-i386
 1aeb33d2dd20015ebfcf1cdfe24be53d8914c009 27459469 main/Contents-i386.gz
 cd9b62e4f2e88457d15d3ffab9bb41c2348c1890 383659523 main/Contents-mips
 7ca717de8812dcee56e285c1f865ddd56458f226 26925567 main/Contents-mips.gz
 b3f0f89ecca89efb0bbdf56794ef397830fa2a20 384238777 main/Contents-mipsel
 a144f2f2e1d12691f576eda64af84baa49b50728 26954916 main/Contents-mipsel.gz
 b6ac6793bbf62409ff23192cab7584ab2fe53ff9 385718105 main/Contents-powerpc
 1a789b026d93b5a3f050e113388df912cead601f 27121913 main/Contents-powerpc.gz
 36ae4b0f6b7bb79eead33cad9b4134877a5de586 378194384 main/Contents-ppc64el
 f996ac36c138d44bd725081da589864534527fdd 26520818 main/Contents-ppc64el.gz
 0f2c68b4823d86842f096df30bdd99511d9ad298 380255909 main/Contents-s390x
 97f3c78cdb5dcebb4e4307c159191970ddb6ddb1 26734701 main/Contents-s390x.gz
 1fa1ffc3fa7a4c9f023eab2b8248d6aff482532f 373524727 main/Contents-source
 a909fd3ec1d0ac42261dc50785c42e1720859151 43001444 main/Contents-source.gz
 fecc07b05eefd42e703704460072107041070b31   349950 main/Contents-udeb-amd64
 edc8a316858dbc14185c935f2717950597fa1e24    29020 main/Contents-udeb-amd64.gz
 cdd2e06f27915c6c52785503d4aeffbe27bf338a   292065 main/Contents-udeb-arm64
 d13641746bcae6e23c50092b89044c99b8fc33f4    25263 main/Contents-udeb-arm64.gz
 3232ed9b4573834a86e9d9820b817159e7162af0   333976 main/Contents-udeb-armel
 d425a66502c27cb467e3653158b5fb12943a144c    27246 main/Contents-udeb-armel.gz
 f40d213e9facd8f6bb11243e765fbef8bbbd2fbe   335334 main/Contents-udeb-armhf
 af741cb0f2d8c8913e9a47d54fb439afc0bf0099    28397 main/Contents-udeb-armhf.gz
 e43e3490d81bab5220330fb6708fcd948eccb827   449242 main/Contents-udeb-i386
 9041ee9c65c96a98e83baca2815ac67f78d6664f    35628 main/Contents-udeb-i386.gz
 06f8fde8bfc7d72cde323217ae797455f3ed25c2   459596 main/Contents-udeb-mips
 99d8a29830516480875448e9cf28ea9110edf80d    35461 main/Contents-udeb-mips.gz
 834e755a46c6678ba907167b6bb229565d5d2f54   577556 main/Contents-udeb-mipsel
 1ed121eb323d8e8b24b43559b0ab957ed55b07c2    42716 main/Contents-udeb-mipsel.gz
 28f9d308df1a08dae30702c472dab376c8f21a63   415803 main/Contents-udeb-powerpc
 cbbfa2f9f20982ae704d7dc000f5d6bdd4fab128    32808 main/Contents-udeb-powerpc.gz
 2521adeda4b314182268043b42c00d608fb77af6   310967 main/Contents-udeb-ppc64el
 143c7694ceeccd339e0119f69f6222e098e22309    26330 main/Contents-udeb-ppc64el.gz
 6390e195706ea58c463ea1f5c820ff84843d4467   271052 main/Contents-udeb-s390x
 b80e4885633fa435e3fa127a1997771a2297c7a7    23599 main/Contents-udeb-s390x.gz
 0bd94f048c730edc1b9714cd67f9f1a9b9ac2fc5 14116035 main/binary-all/Packages
 8609b5127c11e546e0bcf0479216d731037287a1  3927273 main/binary-all/Packages.gz
 0142871bfcab2e3774345ba1f4289980abf265a9  2996384 main/binary-all/Packages.xz
 290307544f19ac40c75c6832221f89fec1b05484       92 main/binary-all/Release
 55145bc9f6e34bccbeac52b6dff973c88679605c 33899150 main/binary-amd64/Packages
 9df4b500e81c45dbbe3897fea5818f7669943393  9049024 main/binary-amd64/Packages.gz
 861bf40668a96e7bf3544bae0b3156c7894095cf  6776408 main/binary-amd64/Packages.xz
 041db98d00906fd06f8e428e592782cf4e185230       94 main/binary-amd64/Release
 7984bf3d8dd4928f8c00216ee7044a94711cea0f 31925593 main/binary-arm64/Packages
 369cc96bf16139d431f8cd8de4c499f35595a7ce  8543977 main/binary-arm64/Packages.gz
 e4fe890b1e97918d07a88721f5845602a468fe4a  6405324 main/binary-arm64/Packages.xz
 09d48fc9c1da873c12d7f51b7b035661a37283d6       94 main/binary-arm64/Release
 940efe6c5dd19063fb70fe1d017da9589b91ebd4 33114041 main/binary-armel/Packages
 037a23b1585a627dedea2e70c3bbf2c2c49b5291  8852522 main/binary-armel/Packages.gz
 17248d10605b1ae0cf6f25d9a703032dc823c8b8  6632496 main/binary-armel/Packages.xz
 0d6969c9f34bd371d867bd12f959beabc75cdbe2       94 main/binary-armel/Release
 f9a2018f898017519a9b1edd985db4031e6eee30 33104737 main/binary-armhf/Packages
 030bb50eea6ba26e3daba52e794987c5a7993726  8849561 main/binary-armhf/Packages.gz
 4d81215e7219a24c616c0957171127d2bbd34829  6631896 main/binary-armhf/Packages.xz
 cd344699cbfbb203f61c111f7c35d2e5c74d2c35       94 main/binary-armhf/Release
 a0331acacd4e8ebd91d35faa5de05de3b9fadbe4 33870550 main/binary-i386/Packages
 94ece2eb0e0834905396413248a41fff6c34c242  9051240 main/binary-i386/Packages.gz
 509d3407572d228efc0c27838443bd67faf5cea0  6779128 main/binary-i386/Packages.xz
 b1de35a9daa354b8282800caa011ab76a76faec1       93 main/binary-i386/Release
 e24cf231ba9d3e464c58525b06eabb1750b4eeed 32797474 main/binary-mips/Packages
 bed228e6bf2b7472d0104b8c4b5c3d889ecf6104  8792469 main/binary-mips/Packages.gz
 932f16ae38605d2c2f4e4246ba00b45ef9c1e4f4  6585720 main/binary-mips/Packages.xz
 f8b34c92b1274f260f7113a71358e6e13c7f66b0       93 main/binary-mips/Release
 d595893542db0d3cf83ff633aa12746760dd5eed 32955178 main/binary-mipsel/Packages
 e342cab69f3215bdc200699e6dd8d346e1072cdd  8817773 main/binary-mipsel/Packages.gz
 41705ca6ff0158fec0a7ea9e217ad133895836dd  6603996 main/binary-mipsel/Packages.xz
 c12b55e61bc94f44a0b573667a8b58df2dcdc61c       95 main/binary-mipsel/Release
 fce93ef30242ea1eefa00d9ccd8d9f2f16aca806 33428595 main/binary-powerpc/Packages
 b9d0028cdca71dbb9d87493cd97d78aee5c110ae  8913905 main/binary-powerpc/Packages.gz
 f0021cd8b9734a680fad3490212cefa6bc2a91eb  6674352 main/binary-powerpc/Packages.xz
 d4379746c9d11171a77ece37ad5dec2e888ada74       96 main/binary-powerpc/Release
 72d80f344d902374f5f0e8ce51e7820e3e5dabfc 32449606 main/binary-ppc64el/Packages
 17f440f7f022d1f442061770aa66013fa8595bf4  8657717 main/binary-ppc64el/Packages.gz
 f27de246746b98d44d34384fe93d3b052f972d06  6487204 main/binary-ppc64el/Packages.xz
 6f15e2e0ea9e46bc6e463cf3827266846abdcc52       96 main/binary-ppc64el/Release
 4c4774610edc5a332f4ce85999dbf452236e8b7b 32646123 main/binary-s390x/Packages
 b48003bc3f11431f30f1f3bc30197670669cd239  8747482 main/binary-s390x/Packages.gz
 ab1ff03bfb783e94679874a2f1784e013256cbc6  6550600 main/binary-s390x/Packages.xz
 57012e02e3176d69ff22d8e51cc3415d9682cdfe       94 main/binary-s390x/Release
 448ac52e1740300bc9b37c1d2f7ec8b98050c770    67861 main/debian-installer/binary-all/Packages
 6c02df734b9167367144bc4dcec1be68f745e12e    19852 main/debian-installer/binary-all/Packages.gz
 9946a8ab5ed5bc9e4573ef931c61b2bca4caa377    17168 main/debian-installer/binary-all/Packages.xz
 290307544f19ac40c75c6832221f89fec1b05484       92 main/debian-installer/binary-all/Release
 ffb226c3db3d24f555324578d50c91af547aa6db   238176 main/debian-installer/binary-amd64/Packages
 80f758f559dbc15b03e1af861df9a01f02914e70    68677 main/debian-installer/binary-amd64/Packages.gz
 54067ff487966e946da978ed531164809d1e161c    57148 main/debian-installer/binary-amd64/Packages.xz
 041db98d00906fd06f8e428e592782cf4e185230       94 main/debian-installer/binary-amd64/Release
 767d2287acc44f27bf862f3bf68444c358bb89bf   221242 main/debian-installer/binary-arm64/Packages
 18f6d2a60ccc933ed5731c73a86970b2b9fa1756    63951 main/debian-installer/binary-arm64/Packages.gz
 032d5cf093ab2fe17b88b470ec37edc04c6678dd    53708 main/debian-installer/binary-arm64/Packages.xz
 09d48fc9c1da873c12d7f51b7b035661a37283d6       94 main/debian-installer/binary-arm64/Release
 83a1038bc7add01f312a9ba3d7d3e42d4cc25bf0   264220 main/debian-installer/binary-armel/Packages
 61a2b2d8852694e5d3f7c6ab418ef4fb41f07741    72013 main/debian-installer/binary-armel/Packages.gz
 ec6d94a1d1e2f3da6aa388ab6c8d3218aadb1a47    60384 main/debian-installer/binary-armel/Packages.xz
 0d6969c9f34bd371d867bd12f959beabc75cdbe2       94 main/debian-installer/binary-armel/Release
 c3dfd3e74491f5fe224533c183d6964174561a9f   223152 main/debian-installer/binary-armhf/Packages
 90dc1d998f1e40146f30a6743f6f9c2f5427496a    65038 main/debian-installer/binary-armhf/Packages.gz
 0856d416c6b333ec0bbb8c6b85e22284505556fe    54336 main/debian-installer/binary-armhf/Packages.xz
 cd344699cbfbb203f61c111f7c35d2e5c74d2c35       94 main/debian-installer/binary-armhf/Release
 47348d6bae8ab8a9adf5fda4cc1a2e065abccf51   276184 main/debian-installer/binary-i386/Packages
 5ca889a7e5acc9977f00db2f3a3fbe3272542aa4    75214 main/debian-installer/binary-i386/Packages.gz
 57dba0ae26e89afc4c70cc75ee738bea25efd971    62864 main/debian-installer/binary-i386/Packages.xz
 b1de35a9daa354b8282800caa011ab76a76faec1       93 main/debian-installer/binary-i386/Release
 0137a0a240f05168636fe7cc100dcd723d6159e9   311838 main/debian-installer/binary-mips/Packages
 9ffb1d67bea9fc66611a45baa4d407ff9b1ce9ef    80395 main/debian-installer/binary-mips/Packages.gz
 92948b2d4ec50245dd06461640865de103a4ecc3    67344 main/debian-installer/binary-mips/Packages.xz
 f8b34c92b1274f260f7113a71358e6e13c7f66b0       93 main/debian-installer/binary-mips/Release
 93a2e641469061224a260be1f2444c123d2e94cc   355242 main/debian-installer/binary-mipsel/Packages
 41b5a29858017fa8e0b178bc7cd5d06e55794974    86928 main/debian-installer/binary-mipsel/Packages.gz
 fcff7b32d26637c88dd5565246cb60ed78ea4458    72992 main/debian-installer/binary-mipsel/Packages.xz
 c12b55e61bc94f44a0b573667a8b58df2dcdc61c       95 main/debian-installer/binary-mipsel/Release
 054e9038264141908e621ec4ec9bf8462b991353   268225 main/debian-installer/binary-powerpc/Packages
 6616188741cb3b236d6683921f1bfb79a9364130    72930 main/debian-installer/binary-powerpc/Packages.gz
 656ad9c5d47a2fe56390284e2746b28919e30e72    61236 main/debian-installer/binary-powerpc/Packages.xz
 d4379746c9d11171a77ece37ad5dec2e888ada74       96 main/debian-installer/binary-powerpc/Release
 e712ac834d3164fe5da7ab1f1a8ae94b51be8a0c   225934 main/debian-installer/binary-ppc64el/Packages
 b9d58ba47997552d59d598d962b47d5e16298eb5    64315 main/debian-installer/binary-ppc64el/Packages.gz
 0ec9b201f5b68ab672077754ff162f0332fb1109    54260 main/debian-installer/binary-ppc64el/Packages.xz
 6f15e2e0ea9e46bc6e463cf3827266846abdcc52       96 main/debian-installer/binary-ppc64el/Release
 c71e2d6129886e67f1c46b8fb69bbbae37537af5   206467 main/debian-installer/binary-s390x/Packages
 1572633d7d16bb15f283125e607e7896c77b76ef    60857 main/debian-installer/binary-s390x/Packages.gz
 d779587b997b344d27cf457846f067a4b533a28d    51020 main/debian-installer/binary-s390x/Packages.xz
 57012e02e3176d69ff22d8e51cc3415d9682cdfe       94 main/debian-installer/binary-s390x/Release
 beff86f2ea1ac3d4f1c4fefa98505e26b9d34964     9627 main/i18n/Translation-ca
 7d9accdb4d42899fb36ef6105bfd4191561fa062     3865 main/i18n/Translation-ca.bz2
 958561d5b050d05dc2431b9373f80468bc874322  1893411 main/i18n/Translation-cs
 e38bfb4f63e22c071364969e6dc14a6b83a696d4   494559 main/i18n/Translation-cs.bz2
 77456f447cbc2e08a37b3ca8403a8889f291dcc5 13166981 main/i18n/Translation-da
 f0afe60ff203a967100870a734ec7f83b28a7073  2901281 main/i18n/Translation-da.bz2
 536b14b84af14ff54b11b55c068a41526256c0ac  7664182 main/i18n/Translation-de
 a389575dde89dfe01261ea7ebf0489198a286c0b  1754517 main/i18n/Translation-de.bz2
 9cefb0051f9503cdcc848113ddd850abc2d8fd44     1347 main/i18n/Translation-de_DE
 c06dc96d1de20832869e5e4b9867e203244228c1      830 main/i18n/Translation-de_DE.bz2
 5d0c67cc346a0bc1a7e68c90c256e8f82221939b     8287 main/i18n/Translation-el
 8af68a2c14ce1de6ad2ddff6a8154a73b6e386fe     1896 main/i18n/Translation-el.bz2
 0ca71a1de96426a00611b20c257cb069175d491c 22326673 main/i18n/Translation-en
 a0a06d06441ace12ff8060c0918297cd482fba17  4582093 main/i18n/Translation-en.bz2
 cb2567cb63e0c6a653ac9f3ee8600f66df3ed2eb     2739 main/i18n/Translation-eo
 06c5eab5a8d8d8906e6b46ca3681353753b4713f     1344 main/i18n/Translation-eo.bz2
 e4f2395ecb214f083f5f03a3d882e01cafabac34  1278186 main/i18n/Translation-es
 e1e2a1507aa22f7229a590ef91866eb5802b9a84   314368 main/i18n/Translation-es.bz2
 ee2bff83bdd46c858bf0b7f4e549090fe9c5464c    17288 main/i18n/Translation-eu
 3f523965a26120bb7136551416d1b985f1b3e447     6460 main/i18n/Translation-eu.bz2
 f56f9484ac31de0c872e38edf1472612bc36c7ea   390801 main/i18n/Translation-fi
 54b5649af6c0167ac40682ee07e7c8225391307a   108973 main/i18n/Translation-fi.bz2
 91d079bed8fb4093c39c68efda8028dec9003a25  3701781 main/i18n/Translation-fr
 e84e6df80f556a880d739cbee04e4ecb10e56b59   846329 main/i18n/Translation-fr.bz2
 374be049736a5babb09e4bf84355fdf1f4b3adba    14962 main/i18n/Translation-hr
 d24212c9372df0c69aa52befae64651bee467fe5     5841 main/i18n/Translation-hr.bz2
 d93eae7e3fb897e28c06aef92bf97173fe52b6e1   101201 main/i18n/Translation-hu
 299635ca125af79bcbac39a0f724f1715e2b24d8    33209 main/i18n/Translation-hu.bz2
 efa4d1d37013def2a438e1fc7d9e6510d7265af6     7753 main/i18n/Translation-id
 4ac4429526acd4c4049527e9a223b67e0d938f75     3093 main/i18n/Translation-id.bz2
 007ee6a728e2a997407ae65b396e74b52c4af0db 17993956 main/i18n/Translation-it
 d0fbfe06ee6c56b95a7dfb877f57608c9d6d000b  3656485 main/i18n/Translation-it.bz2
 dc9e2da571bb679d7d7fff90b0340d588dd5fe73  4376703 main/i18n/Translation-ja
 962786b8489aa58d37de15f5c773f6954941f61c   794698 main/i18n/Translation-ja.bz2
 4a92fec96c8d494094f6840eabc40d373f6fb263    21141 main/i18n/Translation-km
 451f7c324ff46cce9fb40620f29749eb141eddf3     3796 main/i18n/Translation-km.bz2
 1c01e4931669cdc80ea0dd4a135772cb0c471640   924206 main/i18n/Translation-ko
 b7514463ea50fce4020dca15a2ccdc83d7e0250a   204449 main/i18n/Translation-ko.bz2
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 main/i18n/Translation-ml
 64a543afbb5f4bf728636bdcbbe7a2ed0804adc2       14 main/i18n/Translation-ml.bz2
 9fbff159cc2eaf3fd168faf673b9417abda11921     2325 main/i18n/Translation-nb
 0a1d5e9c472db52dfa34ff9d17d3cdd3fed95f65     1302 main/i18n/Translation-nb.bz2
 543cf7bf064c027443108ee08cd5f891e01c2c34   267344 main/i18n/Translation-nl
 a755625497adc7297ae14c25b65541f1b496fdcf    74095 main/i18n/Translation-nl.bz2
 d9bf94b329cd69266232b2506fed296943fb40cd  2490134 main/i18n/Translation-pl
 2bf5473bbb490aefff835f254418eed043668981   574404 main/i18n/Translation-pl.bz2
 478daa98310e8a4db7ae31cb23b0b5336cda8e19  1712133 main/i18n/Translation-pt
 f1f1d4323caa17989f0f01af6b36fef699f66459   413142 main/i18n/Translation-pt.bz2
 b33ad08702a333eb6ed3e31d212f646fa463a04e  3363311 main/i18n/Translation-pt_BR
 bfd5db45184601f95af4586008185cc90659c4c4   802754 main/i18n/Translation-pt_BR.bz2
 0e79d618838b9ecf1e0d8ff3a2834e9fff8eb5a3     2949 main/i18n/Translation-ro
 6edd468ced1103b7ca5647514d47270e7c0b5b07     1463 main/i18n/Translation-ro.bz2
 7d4ebcaa3f7147ab1a68842fa197f6a3f1322ab0  2684275 main/i18n/Translation-ru
 d10b398d2e5099645c7cabacafe333a2a96e196f   437612 main/i18n/Translation-ru.bz2
 8639b1be4e70e09a3ebb52b16a7fd92a95700762  2106842 main/i18n/Translation-sk
 3411db93b2fad0a45efb95f8f9b87e6c0b747c95   509104 main/i18n/Translation-sk.bz2
 f6b79703c7db7d617c9a570c4f43c9ea9eb2ef9d   455684 main/i18n/Translation-sr
 450b9c3d58e36a35c41b0d398734fda78d65ee61    81894 main/i18n/Translation-sr.bz2
 ab7dbfb228d8701c2213a14bab61a461586ed6b7   132973 main/i18n/Translation-sv
 850269802551ff6911a935fbe19484754527ecff    40142 main/i18n/Translation-sv.bz2
 9ce93c94dc38553c9f65d861bd8db0b5570d7bb7      902 main/i18n/Translation-tr
 e7b035cf84219392b6bf5eacec4e629ed7327ace      530 main/i18n/Translation-tr.bz2
 10fbd6905cd8d283bf932cafe38a03d1d63cfde7  4630335 main/i18n/Translation-uk
 c6a136045eadca5b89a278ab333d938749a40c6f   734132 main/i18n/Translation-uk.bz2
 5a3238c961315b4889950678c86c6f3c7548b9e5    36422 main/i18n/Translation-vi
 d9cc8e61fd5a47c7420a3ae9611b42550b9c106d    10243 main/i18n/Translation-vi.bz2
 594f432d94fc9cdc2a871e82ca007497a853b0a8     2799 main/i18n/Translation-zh
 8a1732ac15d1a1a841e2f46dec4747db8a406ea5     1526 main/i18n/Translation-zh.bz2
 d0b34f4b17d101aaf4bb3fcb337e918b2aabca51   367410 main/i18n/Translation-zh_CN
 84124314f4aa1598b05ba88d07c2782611b677d7   100783 main/i18n/Translation-zh_CN.bz2
 7f8df0031bcf78d37e24dcc0d94cbaa6f0f88761    64019 main/i18n/Translation-zh_TW
 a2726bc478ccd8fa368fb1f7a62d5aac0e6c8e31    22485 main/i18n/Translation-zh_TW.bz2
 86ae307bdc516c3a60dda50d369f60ce6a72e430    53815 main/installer-amd64/20150422+deb8u4+b2/images/MD5SUMS
 25e9b782aff96c0c39401fda65ff4dd60444f65d    72131 main/installer-amd64/20150422+deb8u4+b2/images/SHA256SUMS
 51666fa7cc3d8794f0f6800b5737b9811d5aeb54    53815 main/installer-amd64/20150422/images/MD5SUMS
 173a56ee531d9f0d624e5b71f261d970a3c68564    72131 main/installer-amd64/20150422/images/SHA256SUMS
 86ae307bdc516c3a60dda50d369f60ce6a72e430    53815 main/installer-amd64/current/images/MD5SUMS
 25e9b782aff96c0c39401fda65ff4dd60444f65d    72131 main/installer-amd64/current/images/SHA256SUMS
 94808dac96b06d6f76a6ba0d5381f03f8b1d131e    19148 main/installer-arm64/20150422+deb8u4+b2/images/MD5SUMS
 617577875faaced64fbca0bf7795c0617897d065    25912 main/installer-arm64/20150422+deb8u4+b2/images/SHA256SUMS
 f20cac8a79b6054d6694409f4d4589ec1cc9a14a    19148 main/installer-arm64/20150422/images/MD5SUMS
 1efa0211e06c1181d8a77542619bb029a420588e    25912 main/installer-arm64/20150422/images/SHA256SUMS
 94808dac96b06d6f76a6ba0d5381f03f8b1d131e    19148 main/installer-arm64/current/images/MD5SUMS
 617577875faaced64fbca0bf7795c0617897d065    25912 main/installer-arm64/current/images/SHA256SUMS
 595a1f28e67d0c1285a5a15179282fecbcb8df74    11608 main/installer-armel/20150422+deb8u4+b2/images/MD5SUMS
 f78c58c43521506c813ad020118a7c826a1855a2    16324 main/installer-armel/20150422+deb8u4+b2/images/SHA256SUMS
 9662fb7a241d13ea5d2fbb4051180dcb0dacf76a     8985 main/installer-armel/20150422/images/MD5SUMS
 6ff3189348425903c026d8d5772d7be927dbb446    12645 main/installer-armel/20150422/images/SHA256SUMS
 595a1f28e67d0c1285a5a15179282fecbcb8df74    11608 main/installer-armel/current/images/MD5SUMS
 f78c58c43521506c813ad020118a7c826a1855a2    16324 main/installer-armel/current/images/SHA256SUMS
 9147469a52b6bdd1f68a5d683f2606a0245361d9    19599 main/installer-armhf/20150422+deb8u4+b2/images/MD5SUMS
 f4be674120994fb491be398310c30fd8e6ff791a    28379 main/installer-armhf/20150422+deb8u4+b2/images/SHA256SUMS
 91147e2ca3e660bd617c856667f842a9e6a5be47    19599 main/installer-armhf/20150422/images/MD5SUMS
 200cbb2a68833521c2fed1b223d15954c0a2c0f0    28379 main/installer-armhf/20150422/images/SHA256SUMS
 9147469a52b6bdd1f68a5d683f2606a0245361d9    19599 main/installer-armhf/current/images/MD5SUMS
 f4be674120994fb491be398310c30fd8e6ff791a    28379 main/installer-armhf/current/images/SHA256SUMS
 40879443f80cfa1a1a2f878826ecaff1808a19e2    52495 main/installer-i386/20150422+deb8u4+b2/images/MD5SUMS
 35240f2b3a9e101e4e612af932fb8623fdc4ed54    70875 main/installer-i386/20150422+deb8u4+b2/images/SHA256SUMS
 5819c24bab0e04da1fde08013606606b30bf980d    52495 main/installer-i386/20150422/images/MD5SUMS
 79824ace70dd595e26449f35a4d5f0611e1170a8    70875 main/installer-i386/20150422/images/SHA256SUMS
 40879443f80cfa1a1a2f878826ecaff1808a19e2    52495 main/installer-i386/current/images/MD5SUMS
 35240f2b3a9e101e4e612af932fb8623fdc4ed54    70875 main/installer-i386/current/images/SHA256SUMS
 3b90561ce4a2754d5df6589516acc6e7c4fc61af      940 main/installer-mips/20150422+deb8u4+b2/images/MD5SUMS
 3fdf6210280b40a7f9f0f42f937faea21ded5fe8     1496 main/installer-mips/20150422+deb8u4+b2/images/SHA256SUMS
 685f743e7899ca9aaa10f44202ec51f4b5673434      940 main/installer-mips/20150422/images/MD5SUMS
 fc2397431cc35e651022ee3cda702782054482a6     1496 main/installer-mips/20150422/images/SHA256SUMS
 3b90561ce4a2754d5df6589516acc6e7c4fc61af      940 main/installer-mips/current/images/MD5SUMS
 3fdf6210280b40a7f9f0f42f937faea21ded5fe8     1496 main/installer-mips/current/images/SHA256SUMS
 f688765c8fa7b296bf1ff5098d66c9f5943bd1ea     1213 main/installer-mipsel/20150422+deb8u4+b2/images/MD5SUMS
 5ec52e0517fbee0df313290b9f496b4cdaebd90d     1865 main/installer-mipsel/20150422+deb8u4+b2/images/SHA256SUMS
 74aeedd147e8f2eb6612eba145e54f47cfd3241b     1213 main/installer-mipsel/20150422/images/MD5SUMS
 f88473fdff2eb8de57b38583e128f5c61d78b110     1865 main/installer-mipsel/20150422/images/SHA256SUMS
 f688765c8fa7b296bf1ff5098d66c9f5943bd1ea     1213 main/installer-mipsel/current/images/MD5SUMS
 5ec52e0517fbee0df313290b9f496b4cdaebd90d     1865 main/installer-mipsel/current/images/SHA256SUMS
 be8cd148f1e4b0b26d0fba43928f9d88995c0e02     2128 main/installer-powerpc/20150422+deb8u4+b2/images/MD5SUMS
 3557762e511da50790c477d72fb705cafcb8fa3a     3292 main/installer-powerpc/20150422+deb8u4+b2/images/SHA256SUMS
 bbaea21bfb7812a72ea557d17d02a8839e50f15f     2128 main/installer-powerpc/20150422/images/MD5SUMS
 d72b33d3de97e2ace2213b20cd4ea09b990ee376     3292 main/installer-powerpc/20150422/images/SHA256SUMS
 be8cd148f1e4b0b26d0fba43928f9d88995c0e02     2128 main/installer-powerpc/current/images/MD5SUMS
 3557762e511da50790c477d72fb705cafcb8fa3a     3292 main/installer-powerpc/current/images/SHA256SUMS
 2c7b368a5507d14d917b9cf5f6a1aa43f831f2ee      576 main/installer-ppc64el/20150422+deb8u4+b2/images/MD5SUMS
 ff31885a361edf9252e24db2dd266c0b3ca89034      972 main/installer-ppc64el/20150422+deb8u4+b2/images/SHA256SUMS
 43039a14e688a53d77ffe80c42edc95d88049c22      576 main/installer-ppc64el/20150422/images/MD5SUMS
 8a074f5e6df3799d0ad2ce8d93070459f0a1318b      972 main/installer-ppc64el/20150422/images/SHA256SUMS
 2c7b368a5507d14d917b9cf5f6a1aa43f831f2ee      576 main/installer-ppc64el/current/images/MD5SUMS
 ff31885a361edf9252e24db2dd266c0b3ca89034      972 main/installer-ppc64el/current/images/SHA256SUMS
 2f9d1faa95847b56b82ea1768fd01fab7031b4f3      374 main/installer-s390x/20150422+deb8u4+b2/images/MD5SUMS
 eee089058b9c1684a9c480dcffe66ba1209fd1fe      674 main/installer-s390x/20150422+deb8u4+b2/images/SHA256SUMS
 3bf34ed8abff354c28df016edc7cd03e0a84d28a      374 main/installer-s390x/20150422/images/MD5SUMS
 c4a693e53b07e052eba56875b756236c1d564fb3      674 main/installer-s390x/20150422/images/SHA256SUMS
 2f9d1faa95847b56b82ea1768fd01fab7031b4f3      374 main/installer-s390x/current/images/MD5SUMS
 eee089058b9c1684a9c480dcffe66ba1209fd1fe      674 main/installer-s390x/current/images/SHA256SUMS
 34847ee22578671220255adedd50593f67c808aa       95 main/source/Release
 3df5e2a32e0cebc84510bef06b4687b32702a7ff 32518618 main/source/Sources
 faabeeade3f39c1655d7d794b035afd5d956ef0f  9156980 main/source/Sources.gz
 9c0e500d5e345c8e376e45f4bfba7d5fb5f42038  7055888 main/source/Sources.xz
 c36d84d59482c2cba1bacd3e7ad27ab9c3a8ea1d 13875760 non-free/Contents-amd64
 acd064d33d6bfe4d839c9e53d1a65dd9d432abec   780181 non-free/Contents-amd64.gz
 85d7589ca0aef6fe843b9c6cdcc4eb93c24094c0 12901760 non-free/Contents-arm64
 a30eba2aa5a32f2a18402ca5cf944010211e1c06   702742 non-free/Contents-arm64.gz
 2c4d1faa9e30409b4edbea302936c622ffc6834e 12974290 non-free/Contents-armel
 ceae77d1c7e9f6e3264dad770c22eafefef95e83   709306 non-free/Contents-armel.gz
 775d32790d52ac964d7ce450e0e018635dfd44c1 12999757 non-free/Contents-armhf
 dc7fc9f63f94f750e8b60a8cb7023369af36c737   711618 non-free/Contents-armhf.gz
 0097df04452a43c6812a8990acc4373808d95b78 13788194 non-free/Contents-i386
 97894317bb5a808f5bf3cc82c698a22197af7b8b   773196 non-free/Contents-i386.gz
 522bef06411135332a0fee5df097c24030eb178d 12988374 non-free/Contents-mips
 b174fdd0a6987054e910fb85dbf7cde0d664436c   711304 non-free/Contents-mips.gz
 2444014a26278fe02fd5ef95efc4c91d85c2ddb6 12993364 non-free/Contents-mipsel
 b3246cc153766c22281539a06046f657eb13c467   711343 non-free/Contents-mipsel.gz
 0bddf1d5f58326b2177d4902b1d112544e010056 12984126 non-free/Contents-powerpc
 563ff66a8371709500e56d17bc075ccaa4927cbd   710607 non-free/Contents-powerpc.gz
 0b8e267f48eeb10871f168f0ff3338e25394a59c 12948073 non-free/Contents-ppc64el
 8dc08c5ac01c13cfe34f6a2809e81a7681cad353   707183 non-free/Contents-ppc64el.gz
 195e15f4e8a7c177c48e53a735b3fcf6e663c0b5 12985547 non-free/Contents-s390x
 f30d25c0cebe4732d450c273aa7f2a0361d42ccb   710715 non-free/Contents-s390x.gz
 9ae22006886886adf5f2f69e690902ddcfe4fe35  8565950 non-free/Contents-source
 50b6a6a2a8edd93397dfe7cc8712af1e0df8f24e   917043 non-free/Contents-source.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-amd64
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-amd64.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-arm64
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-arm64.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-armel
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-armel.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-armhf
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-armhf.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-i386
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-i386.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-mips
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-mips.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-mipsel
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-mipsel.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-powerpc
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-powerpc.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-ppc64el
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-ppc64el.gz
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/Contents-udeb-s390x
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/Contents-udeb-s390x.gz
 b4c93c35901b47802933b082707ab5b5743a3a5a   190308 non-free/binary-all/Packages
 9e24db24381a0685db6bcae64dd518624de317dc    56603 non-free/binary-all/Packages.gz
 34a05d9a7d4e09aa0ae864181cadaa202a8be6b5    47968 non-free/binary-all/Packages.xz
 904aff34aee8e4817de41187187352b53f1a8a07       96 non-free/binary-all/Release
 c236fa995bb8b66e9580e046e63694cce8c70232   355467 non-free/binary-amd64/Packages
 26d8e5f933cedece70c29d478d72ad2f793c39c1   101206 non-free/binary-amd64/Packages.gz
 79da65a83d3ae6141b578a5dc8bb6eba35e38aff    83648 non-free/binary-amd64/Packages.xz
 dd81ba598ee1b0193fbd0a67d8c6416c581c2f88       98 non-free/binary-amd64/Release
 23841a96f46a0d7cddccb0780bbb65b202d5592d   230493 non-free/binary-arm64/Packages
 7cd94971244d0a939396908c14ace42f933ae09b    68723 non-free/binary-arm64/Packages.gz
 918724f9367024d08f0f97a79217dadfb61a6acd    57432 non-free/binary-arm64/Packages.xz
 30400b4433051da2cc47fd687b87a192c4f6d644       98 non-free/binary-arm64/Release
 d66e819609dce5c5c1f6695c8086c7e2a6de40a4   239168 non-free/binary-armel/Packages
 09b9e5c1f690352684a12da5e83010ca5d4f08d8    71096 non-free/binary-armel/Packages.gz
 ebe7c48d7b8c76075f9d464031f4128b64740e2c    59656 non-free/binary-armel/Packages.xz
 92b6270430cf9a6dd763f3e3a48fedb424974580       98 non-free/binary-armel/Release
 745c10505313bf575a67e23c79e544b636aa09bf   255719 non-free/binary-armhf/Packages
 50112a64a271ad269b5d657273f339d4a5fa894e    74897 non-free/binary-armhf/Packages.gz
 4044d8700259e6714d1909f2b4d2fbbb490af3b9    62484 non-free/binary-armhf/Packages.xz
 fc8b439db572a9fe01088f5b826cf27b17c60e02       98 non-free/binary-armhf/Release
 28a8e45d668e5a06663a4457eaa8270e2e89873a   344437 non-free/binary-i386/Packages
 b31b6d179fd4f8b20c7f6a63e7ca34a77b55b66c    96963 non-free/binary-i386/Packages.gz
 390a0bf1b913e55fbb788c89bfbc606a16c068a6    80432 non-free/binary-i386/Packages.xz
 118a448356ac9459a4ee93a4bea467f46991528d       97 non-free/binary-i386/Release
 675c73caadcad7fa2dabd0247f9b99ee4312a0e7   240596 non-free/binary-mips/Packages
 a7bc45bf31b0f6d537a33657009ee967a2943f28    71671 non-free/binary-mips/Packages.gz
 ab299b0f4a3f5862718b292df1a644e70487cc5c    59864 non-free/binary-mips/Packages.xz
 253a3866eda450aa4a55ec756a67cc59dbdb954a       97 non-free/binary-mips/Release
 90b718f480410350319088c77c4ede782a4b7b98   243099 non-free/binary-mipsel/Packages
 1e1b30a3e3f6612989fddcabc97d8c400a942719    72082 non-free/binary-mipsel/Packages.gz
 51f14e467dad302908fb3156a289efa0b3953ae3    60296 non-free/binary-mipsel/Packages.xz
 89e010bf2f8656f5e29243ffc585c24f42abba8c       99 non-free/binary-mipsel/Release
 b96d97936b3f8b9c27b4584df85566eb724610df   239066 non-free/binary-powerpc/Packages
 7d263663ff9a506e8cfc015b4469e7952cc05352    71145 non-free/binary-powerpc/Packages.gz
 65b241ac0074604e89a40c0464a06ba71ea08dcd    59496 non-free/binary-powerpc/Packages.xz
 84050b59ace9b6efce65f7c8869f4790653be7d2      100 non-free/binary-powerpc/Release
 3cfa513e06baa4e696f5db58a1d59700f9ee9582   233710 non-free/binary-ppc64el/Packages
 06b1a0866301cee3d23097ce291874a4b202e438    69140 non-free/binary-ppc64el/Packages.gz
 76f0f58bc54e81f6494674cfdda7f8c066094889    58056 non-free/binary-ppc64el/Packages.xz
 ce6c249029c6f3c25045a0526ab0ed81e5e3f36b      100 non-free/binary-ppc64el/Release
 b8127c8b686437303a4a9c1fe4a2c5a1fecaf574   239446 non-free/binary-s390x/Packages
 4cfbd523d9a9de3fecfac8270b42d25a714caf02    71176 non-free/binary-s390x/Packages.gz
 a5ff4de05caf7bcd8505bf7d1c72557b8d32d28b    59472 non-free/binary-s390x/Packages.xz
 c1eb0c3fed326d52dbb24b8417ba4bd63fa46945       98 non-free/binary-s390x/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-all/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-all/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-all/Packages.xz
 904aff34aee8e4817de41187187352b53f1a8a07       96 non-free/debian-installer/binary-all/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-amd64/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-amd64/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-amd64/Packages.xz
 dd81ba598ee1b0193fbd0a67d8c6416c581c2f88       98 non-free/debian-installer/binary-amd64/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-arm64/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-arm64/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-arm64/Packages.xz
 30400b4433051da2cc47fd687b87a192c4f6d644       98 non-free/debian-installer/binary-arm64/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-armel/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-armel/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-armel/Packages.xz
 92b6270430cf9a6dd763f3e3a48fedb424974580       98 non-free/debian-installer/binary-armel/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-armhf/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-armhf/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-armhf/Packages.xz
 fc8b439db572a9fe01088f5b826cf27b17c60e02       98 non-free/debian-installer/binary-armhf/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-i386/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-i386/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-i386/Packages.xz
 118a448356ac9459a4ee93a4bea467f46991528d       97 non-free/debian-installer/binary-i386/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-mips/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-mips/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-mips/Packages.xz
 253a3866eda450aa4a55ec756a67cc59dbdb954a       97 non-free/debian-installer/binary-mips/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-mipsel/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-mipsel/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-mipsel/Packages.xz
 89e010bf2f8656f5e29243ffc585c24f42abba8c       99 non-free/debian-installer/binary-mipsel/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-powerpc/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-powerpc/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-powerpc/Packages.xz
 84050b59ace9b6efce65f7c8869f4790653be7d2      100 non-free/debian-installer/binary-powerpc/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-ppc64el/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-ppc64el/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-ppc64el/Packages.xz
 ce6c249029c6f3c25045a0526ab0ed81e5e3f36b      100 non-free/debian-installer/binary-ppc64el/Release
 da39a3ee5e6b4b0d3255bfef95601890afd80709        0 non-free/debian-installer/binary-s390x/Packages
 a0fddd5458378c1bf3c10dd2f5c060d1347741ed       20 non-free/debian-installer/binary-s390x/Packages.gz
 9746882f4236fa1c3a8f86be2f1d9c46680c0b10       32 non-free/debian-installer/binary-s390x/Packages.xz
 c1eb0c3fed326d52dbb24b8417ba4bd63fa46945       98 non-free/debian-installer/binary-s390x/Release
 793c888b7347b5d7276eed61df40518e64e1b011   309527 non-free/i18n/Translation-en
 991ba4e60cab9834b47ef457579a56408a2604f5    72067 non-free/i18n/Translation-en.bz2
 1ec26c4d23e425c3963a734a30395e6d11624e10       99 non-free/source/Release
 cf48b1e1477ccf1a4ae48d7bcd78b6feb07922f7   397177 non-free/source/Sources
 ee1c5cbffe42a6db32776c931963629bb8307903   119076 non-free/source/Sources.gz
 ab3e37b419cdb2e40aff16cd3675604e3dc7f025    99496 non-free/source/Sources.xz
SHA256:
 70dfde0e0dacb43d6c9504cc09159d69ed62e046f8749b539f52d001faf3dc78  1194094 contrib/Contents-amd64
 d566f018280f640d5f2b6591563a508af32eda940a4e62f7b17a5c16d2d3769c    88313 contrib/Contents-amd64.gz
 1dcf1464af08c9ec2af63a90c4ad8eb7aa2c35f52cd2088b2fb1fc5db9039f50  1021028 contrib/Contents-arm64
 eec43d0550b4b3b1cc35cacdad8a8a2bdd96bbf19ddeca76e5e345687294b2d7    72294 contrib/Contents-arm64.gz
 1dfa7b71670e9f494401db5dd893c7946456c87e15743900ae10595f501c48ed  1035060 contrib/Contents-armel
 5b8495de1cf1ea32020e1b5856e13476d5e9b4230a0554ace22994ef04997a78    73507 contrib/Contents-armel.gz
 a8482be17180d6d6c3382b1e0dc55caf9c48e186790372ccdc5df6bf6a1710ec  1027963 contrib/Contents-armhf
 5de5db1982f5cadf0ff07f7671b57852189d96dae29330bf65745dad8fe23164    73418 contrib/Contents-armhf.gz
 e1c230f21ac85b921a2d65516bb8cb47e004155c92d8ba4c2ffab7e48a9e1ad9  1190658 contrib/Contents-i386
 defaba7dbc77a80bf697760ee413ac329a595f579b77c5f6232913a83d878d6d    87973 contrib/Contents-i386.gz
 cff1d647d964d641b388292d35d2bec4300909044ccc416b2009f492d987a038  1036779 contrib/Contents-mips
 55bcf94ab552b1fc58a9dbcabb17f44cb5b3d180931f2a8474a5c58e480ab85d    73599 contrib/Contents-mips.gz
 cd211368e3164027a70d57b7f0d210f2573fc627bb62576969b6399fe100168d  1036785 contrib/Contents-mipsel
 3472a4a58f4fad30b26eac9c74a635fa032fac66c14ab1548471abc8e6563cfc    73704 contrib/Contents-mipsel.gz
 c93d042b6d0973897e4d8108273bf8a288384c752926a573b4997a4ba7d89695  1037497 contrib/Contents-powerpc
 e982f7b02a276d5bc9383dd25cd522cbd14bcd35657b49aeb9738263dc71303d    73868 contrib/Contents-powerpc.gz
 603dac5d8370218a6b2f8e0f5a1df23d0e21ae89988a6254b6ff794cead66ec6  1017446 contrib/Contents-ppc64el
 2497d2625aa626bc711a07be13be29f4c84abd2d3c5b1306d7be834dd3eb8c3d    71910 contrib/Contents-ppc64el.gz
 8122ecc91ec253bc8e68268abd17a1a320e78dff411deb8e7889d0f6f818bdc0  1032989 contrib/Contents-s390x
 0db1d92dc4b393bb53685b8001f4ed08b312f61b1b654434e4af49f7d15f3e14    73259 contrib/Contents-s390x.gz
 d0f42d838b63a8bd067ad693eaa9f78b7fb492a5f25f4faac0e277814c25959d  3015028 contrib/Contents-source
 d13a5914bcdabd845ac1047efaa3bc24e3f23a44eb35e28313b5c857a518dfc0   334510 contrib/Contents-source.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-amd64
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-amd64.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-arm64
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-arm64.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-armel
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-armel.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-armhf
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-armhf.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-i386
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-i386.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-mips
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-mips.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-mipsel
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-mipsel.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-powerpc
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-powerpc.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-ppc64el
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-ppc64el.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/Contents-udeb-s390x
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/Contents-udeb-s390x.gz
 2826aaa5a00c78f02fcda1315188ffe48451a560607bd5011e2a772bf0c058a6    84184 contrib/binary-all/Packages
 9773db087812d435af9cbfc74e8eef2345dcd1a486298d41ce77110ab934685e    27124 contrib/binary-all/Packages.gz
 0bf2b9e120bc26a896e41c17d88107b22b8b1dd20a018a937a3d9a9d2fb0224c    24000 contrib/binary-all/Packages.xz
 e8cc10d377e9eb57e16929396f6d45d7d7704ecb1a7fd97e75ffa0ce4c2b3837       95 contrib/binary-all/Release
 763f5212b81a943fedbf01f357c9dd9f63ad020ce30deadd8a5eea7cd50ec4f9   197960 contrib/binary-amd64/Packages
 8dd7c4d79f269dc12c964328a97e39083eb953855fa77517c6fa8c834c514e5a    59500 contrib/binary-amd64/Packages.gz
 721c1a95a9af56f0143421cd6a3acae525684d19ae925a12962122f7223879a6    50168 contrib/binary-amd64/Packages.xz
 61b1482e54287b83e4ff70d641c3a3de652936cf9648c8e4888dc28770e810d1       97 contrib/binary-amd64/Release
 bb950a4dc569d7c906f7c7045f0cc0e29ca1eb404a676bd9ead9dfa4a11127bd   133508 contrib/binary-arm64/Packages
 e36bdd4684f359f99efea5bdb2c6b451ad02d7b0ba87cfc5d16131c4f4147fd8    41575 contrib/binary-arm64/Packages.gz
 9e002dac6baea359ab66ef91574d82e188f4c7d97c7bea25b3fce497404505bf    35840 contrib/binary-arm64/Packages.xz
 13f731b60ff2d834ac26f5f3f412b058fe6e604f2e2ba5554539cfb58033dd3b       97 contrib/binary-arm64/Release
 7c9e5fdfb203b655303f246aa54b6540c7bdee2731b9a84fd4620519a1212ebd   139204 contrib/binary-armel/Packages
 54ea610c62ded2b137dc8cff84505147b791032f9095b8bb0d8262dc5dc31ea2    43258 contrib/binary-armel/Packages.gz
 98bff095cae55ebf342d7e5c7695d9ea411ac8876e0fda3819781a36b915a50e    37100 contrib/binary-armel/Packages.xz
 bbaa67e7a5782b537f5fa1f3377f67bbfea9f5926b581e8777edd720888dd3a1       97 contrib/binary-armel/Release
 832bfdb650df17f7ce87bb6b1efbcc7bd81e1c1208ac7edf1444002ec1928e02   144473 contrib/binary-armhf/Packages
 4f482968a213787f1cb42c85d23c9f7e567dd8984cde402f9fabf69f292a0175    44700 contrib/binary-armhf/Packages.gz
 3257b7d0b6e3ae7d32fbd0ac8fc9752b3894136bb79b803cd00564ff1331b78b    38136 contrib/binary-armhf/Packages.xz
 d9718953dbd5cff7c0b654ad5c08f6f4b231d1b1509539feeca37fbc23096c4d       97 contrib/binary-armhf/Release
 0a5918dd753ab387811988353e5e54a75fa08ab7c9f47f367f44b38269fad2b0   194806 contrib/binary-i386/Packages
 fe6bbc5939e263ca2023e3cf84b1038d609006f40c319751efe18f350808a518    58600 contrib/binary-i386/Packages.gz
 ae4f7eec55cef8592618c50533a9d5b42ce77d327c41908a2930076e632bea8c    49492 contrib/binary-i386/Packages.xz
 720801e376a0f66fd9f21ed46cd35d5dd1dfcff2cdda96084d7a95e75851ab83       96 contrib/binary-i386/Release
 5ff2fc56a5e8a43ef8bacc8a1d1126792bbb242b9bddf7bcfedf4a7224aa821e   140865 contrib/binary-mips/Packages
 f7756b9280760f43adf50a61615803763088d21838476a17cdcd620d7ed31150    44000 contrib/binary-mips/Packages.gz
 f2d518ed438af77220e6768167e1a0ae5652adc3c179ac1100ad6a9c2f845433    37540 contrib/binary-mips/Packages.xz
 42a15f6f6b967d1df06f3873044fd32563987109493c387fc05288c025e7890e       96 contrib/binary-mips/Release
 ef8ea2f66b28877f8eef39c902791057da83ea230e7d5af68208c8c48b8882e5   141128 contrib/binary-mipsel/Packages
 feac53df0c7414f89ae4e60491cf17802d70cd4d370abdc76d7465c725cb5a1f    43785 contrib/binary-mipsel/Packages.gz
 43c1c9eed68cf573b25d0e174f3c9807233a72035580b385592d1f77cf0f4df1    37568 contrib/binary-mipsel/Packages.xz
 717ba339bc4f536af66d743ad627ce1f7016f78993cb121d789ca07bebfe4921       98 contrib/binary-mipsel/Release
 0ac05ff4ad7f29bc5c20a4b088d89a5e749abb38d720459caf3e18c7bf4c9d35   141881 contrib/binary-powerpc/Packages
 61e88101f8731603c48565dc797517c5a22851cd81156da8025e85534c54071f    43955 contrib/binary-powerpc/Packages.gz
 a7cf2de9017cd120d63dd373bddcd310e360fa50dc132ed59c52529d2e6b9b64    37700 contrib/binary-powerpc/Packages.xz
 ba662513af08a2e0bc24ded87b5ac626eebafcc4cab4d047b28c5439e8440589       99 contrib/binary-powerpc/Release
 5aeb329467b1023594a6e412f79a5c1c3f8e3a342b3daeda84bd39cc5f739ca7   132753 contrib/binary-ppc64el/Packages
 62f4e178677361d1ba3ab2ad4678e0c4e6114361e1d99acd61a7e17777eb6014    41486 contrib/binary-ppc64el/Packages.gz
 965e069388402708ca5988fedda5a04071b5433f9b673cce9c4370530b3714f0    35696 contrib/binary-ppc64el/Packages.xz
 88644611089326431ac9f9c5904d83388ec80fe916d4d84cf68e314b08638604       99 contrib/binary-ppc64el/Release
 c37108d293943b8fbc3792028f34fb3fc8d0ae27c7777ec6657ee4285d876270   137757 contrib/binary-s390x/Packages
 a1f25cfa67366c82e09fe6af96f18dda5f987ea2a7bf7b276059d6ad29ac454a    42871 contrib/binary-s390x/Packages.gz
 7317581e8b300f4f61a243e246ef8b322cb305d338ddebe985ff8292a962a7d0    36764 contrib/binary-s390x/Packages.xz
 cc610f29842f1408d7f5a88ed35625ed3896c69231471d176d4d64aad1179b8b       97 contrib/binary-s390x/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-all/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-all/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-all/Packages.xz
 e8cc10d377e9eb57e16929396f6d45d7d7704ecb1a7fd97e75ffa0ce4c2b3837       95 contrib/debian-installer/binary-all/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-amd64/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-amd64/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-amd64/Packages.xz
 61b1482e54287b83e4ff70d641c3a3de652936cf9648c8e4888dc28770e810d1       97 contrib/debian-installer/binary-amd64/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-arm64/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-arm64/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-arm64/Packages.xz
 13f731b60ff2d834ac26f5f3f412b058fe6e604f2e2ba5554539cfb58033dd3b       97 contrib/debian-installer/binary-arm64/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-armel/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-armel/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-armel/Packages.xz
 bbaa67e7a5782b537f5fa1f3377f67bbfea9f5926b581e8777edd720888dd3a1       97 contrib/debian-installer/binary-armel/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-armhf/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-armhf/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-armhf/Packages.xz
 d9718953dbd5cff7c0b654ad5c08f6f4b231d1b1509539feeca37fbc23096c4d       97 contrib/debian-installer/binary-armhf/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-i386/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-i386/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-i386/Packages.xz
 720801e376a0f66fd9f21ed46cd35d5dd1dfcff2cdda96084d7a95e75851ab83       96 contrib/debian-installer/binary-i386/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-mips/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-mips/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-mips/Packages.xz
 42a15f6f6b967d1df06f3873044fd32563987109493c387fc05288c025e7890e       96 contrib/debian-installer/binary-mips/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-mipsel/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-mipsel/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-mipsel/Packages.xz
 717ba339bc4f536af66d743ad627ce1f7016f78993cb121d789ca07bebfe4921       98 contrib/debian-installer/binary-mipsel/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-powerpc/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-powerpc/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-powerpc/Packages.xz
 ba662513af08a2e0bc24ded87b5ac626eebafcc4cab4d047b28c5439e8440589       99 contrib/debian-installer/binary-powerpc/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-ppc64el/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-ppc64el/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-ppc64el/Packages.xz
 88644611089326431ac9f9c5904d83388ec80fe916d4d84cf68e314b08638604       99 contrib/debian-installer/binary-ppc64el/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 contrib/debian-installer/binary-s390x/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 contrib/debian-installer/binary-s390x/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 contrib/debian-installer/binary-s390x/Packages.xz
 cc610f29842f1408d7f5a88ed35625ed3896c69231471d176d4d64aad1179b8b       97 contrib/debian-installer/binary-s390x/Release
 5038a0b7a1700defc6a56562420c6fed166cd6066d7582770131b298170fb4b7   144523 contrib/i18n/Translation-en
 cd0cbd0c96469fdbc197117a4b9e1353b73930e31fcb4ed40abda369d5234e8a    38528 contrib/i18n/Translation-en.bz2
 8c5dec5bef01dba70486040eb7dafcdd4d96df3e5d5de7eb3582b1fa18832dfb       98 contrib/source/Release
 3d58220ea0e8c31c30d2e29c312a3358707f94669d998db99c0a2843c4868259   191039 contrib/source/Sources
 4f3f91d02a672a33aad6f1dc403a4de4c0807746cf5b660f091300d01b230982    59528 contrib/source/Sources.gz
 d5a3210df7f10af491760a7d22b2ae5c0fd35bce86f19536c4f967bdeedce021    50796 contrib/source/Sources.xz
 2f42d98c04757bfc27f03835a09514cfe54d330cbd22cb4c1110586ca084db4e 388115371 main/Contents-amd64
 ac659840faa6a4495a3fd2cbd11d0b81adcfebd8eec29a2134240bb75b142f59 27347433 main/Contents-amd64.gz
 e2cb770011e5a3ceec0f1304c8ce8a8e61c40623422e22947db258b3ad63d043 377276368 main/Contents-arm64
 1ce1f61aabe860f46b324d37acd34acf902e4511b7529032caf233ab8fda5838 26457779 main/Contents-arm64.gz
 0fd863b61cf4ea0bc2155a2cb2b71c31f34dca0f9be7d910222b1d307b6a2678 384735136 main/Contents-armel
 dc8fb78283420e1365be147d74c1115ca4690371b71668d35edf7e788d6be810 27017480 main/Contents-armel.gz
 83174da7d179394bc826d420fcb56e101ed122dc9a8e21adf1971f17756b4d31 384395929 main/Contents-armhf
 bfae8675026b43d5159044f508c3cd187391109463ed2fa18878c32b1d60947e 27006099 main/Contents-armhf.gz
 d4bd66168e3e2933fb75b499675a9f2967a364db7f902825560a0e4451274eee 389537589 main/Contents-i386
 dc682daa7ce7cc10b3cf61359e434a0b8fdb394fcf6bb90210d5b8d2c409fb86 27459469 main/Contents-i386.gz
 564faf4275135a0331b888b5b7a3cfbf45fac09615d5114b473b8945547b44eb 383659523 main/Contents-mips
 4b27f62542fd9bdd4e14f69990751e48a3c011d34844e0b36a3b9db3cfac7f98 26925567 main/Contents-mips.gz
 35fce3a6cf89ec7241f379bed218f269920b51949361783accb82d0ae95e9d07 384238777 main/Contents-mipsel
 e9c67f9a62124a3655d9d7ba7bd75db22dfaad605fab132ac3600fb2739585c9 26954916 main/Contents-mipsel.gz
 0940a80694b9c1b54c263f1785d988ebc832792a0ac8869c5c6d9adc4f3ef0ee 385718105 main/Contents-powerpc
 8e9fe24ae9bd56d3f4b7668094f1f17c0ae60e5421fe675368fd9b3376ce6cc0 27121913 main/Contents-powerpc.gz
 6f41448b05dc09fa5561b277e2518b1c026fb4b27f7fa428c81fdf543ba0475f 378194384 main/Contents-ppc64el
 b0d7b2c9612ec8ce5a67494d7f8198b9158060f4b39e9de794a7109b9e9312e3 26520818 main/Contents-ppc64el.gz
 125a66ffe0c1076c10527af4f18b1b222e9d514334ce3ca529dd5ce2090b0a06 380255909 main/Contents-s390x
 5d991b294b777ecc2b84a66886bbd0d3b9c586f102ffcab10ff15a5202abcd27 26734701 main/Contents-s390x.gz
 6696e2651fd6550ecbac6eec36090fa85a2523524416c613308b7bd0c27bbd8c 373524727 main/Contents-source
 734525eef936773da84ce63abf758196d632d09d01f3be934c132f22384d8282 43001444 main/Contents-source.gz
 88c87c5fcec0d50ddc04331200ec4401076954a16156fe438fe1b71a5b30db33   349950 main/Contents-udeb-amd64
 c512148c2c9ec9bdfffdc10cf5ec5df75cd704c60267f1e539bb8cdacfa1ec32    29020 main/Contents-udeb-amd64.gz
 710896340d1c1bd2639c92ed9398e5553b09b3b58fb0ab1afe03db9538baf93c   292065 main/Contents-udeb-arm64
 73e12af2d4492e8d4bbdc1276f93e4e3959d7f5d3851e9450c024f1cfed432a9    25263 main/Contents-udeb-arm64.gz
 81b5f03bf63e76a76b938d7c29d7ee9d033e4718c41fa97b5acde18843fd0c9f   333976 main/Contents-udeb-armel
 4dbecabf3646ac0aa5b1c81efbf1f8dd923e80b844ec7c2dc4fab514e83076b1    27246 main/Contents-udeb-armel.gz
 9eb2cf135c9fe4fd535309961da2611bfe44164e1c21dbb1581dfe28135f4088   335334 main/Contents-udeb-armhf
 765c2bff35f43106b8321522c0b18a27da771a4f33f9e9cab82dcade133a33b8    28397 main/Contents-udeb-armhf.gz
 c5c670f511dfb7e6327586b285afd8bbd4dd3e6f1f36ada317bca3dcc24924a3   449242 main/Contents-udeb-i386
 e761965a4871ab3a5e15f48095f2af3251fcb9abc8a3c71a08a26d01e245139b    35628 main/Contents-udeb-i386.gz
 fe71988293f82acb163a74bb69fb3d659c31e906ecd0aedce0ed401b26c0f61b   459596 main/Contents-udeb-mips
 74f2df052c240d6820f34c63d6c91f6a97383bbefc822aec965aaa0dbb51c7fa    35461 main/Contents-udeb-mips.gz
 ecb0775b9c7bd31f283db099b9b13d7e7006237e6274378afafc4f217c23857c   577556 main/Contents-udeb-mipsel
 b6cbc2e561b5450a9a2b4961e988e3a0ae8b1e8e37c8eec69b5c4f55ad31cb32    42716 main/Contents-udeb-mipsel.gz
 7c4838317b1e8d07389f9643b19d8328143b63ec8a75d7ef8c74a36aade9eeac   415803 main/Contents-udeb-powerpc
 6365c9051f7d4b055a6514ef71cf260a46d7e22025190df1c3f45016d7882986    32808 main/Contents-udeb-powerpc.gz
 0db08ec3c688dbf32c6da140d904528295cc11293fb854fbbeb05877b731081e   310967 main/Contents-udeb-ppc64el
 62e52da765c80b48e9c670c366d353c00098039a80ae9aca4683870ca56aaf3a    26330 main/Contents-udeb-ppc64el.gz
 65a4d6b3cb69b64478cf570cc65d3648a961241acbb174868c2f000da3f0e547   271052 main/Contents-udeb-s390x
 21a160d7dcf3ced3d71486927931dc10243d045c718441c5e96a18d412746144    23599 main/Contents-udeb-s390x.gz
 356f396a8673173bb2f7d699e2d2813aa7edd1581465288d2c5d72830b20ea21 14116035 main/binary-all/Packages
 507cd3340477f646525811f7d6cde86139c61685c877fc31fbe9b259f326ed3b  3927273 main/binary-all/Packages.gz
 16ca7d44394446fd84683a48c122da45bdf42ebd78cc709608fbb884ef8f6cae  2996384 main/binary-all/Packages.xz
 14419ac338393d627e857115b7528c0665fe12eae1d8330f9ee809b0955ccfa3       92 main/binary-all/Release
 4b2fad8fbf2d5fba2b986c8a5ce97b0f26ad734919ec296794584ae203a80717 33899150 main/binary-amd64/Packages
 33bdf272ef03050e558315236734c30aada851e35b651df05e1a3eec8f689b30  9049024 main/binary-amd64/Packages.gz
 b4cfbaaef31f05ce1726d00f0a173f5b6f33a9192513302319a49848884a17f3  6776408 main/binary-amd64/Packages.xz
 2c4f0f20a2503fdb19c68d2c37a6bde9cab84a595408d8d01cef80bb6927525c       94 main/binary-amd64/Release
 d953142bbdd0457693deb4115ecfdd6800fbe614445967f04c2c8a04de5c7463 31925593 main/binary-arm64/Packages
 4da59761329c6390bc91875349a18f729a90d818c509b2ced738d77261319e30  8543977 main/binary-arm64/Packages.gz
 80cb8436cf323c6fe3c3913da8f7cb0f15bde09b720159b29bae14d902eb814a  6405324 main/binary-arm64/Packages.xz
 4eba3bfaead4bec4aabb8b9fc964b843e69b8e2da567bbadc73e62f7b2f1f562       94 main/binary-arm64/Release
 dc2e700bed252070abb8f95e2eb3aec9c1281050c840bfa88146372d20d3dbb2 33114041 main/binary-armel/Packages
 da9fc63610304e97e33b8b2a114c91bab2ebb4fc80bfd8ef102b2d3428952432  8852522 main/binary-armel/Packages.gz
 38a2f0970a78f57b9d003b19b1a0eb867971bfd26ac5b365399fef84b00bfa37  6632496 main/binary-armel/Packages.xz
 e6afdec88f50b342ea5be05e15100c34cc9de4737633d02310b7a4fc05e858d3       94 main/binary-armel/Release
 05108985b527aafc330ac6d9afc2b64057212cfc27096545648510d0239cbcfa 33104737 main/binary-armhf/Packages
 05560aa8bf74d1350d1e07fac6b097924bf7bf174c6dc7d902276e8d1d7b7cf9  8849561 main/binary-armhf/Packages.gz
 390f5e0abb5aacbd54054a4cf97009b89cb6295b60434fc00d4ea6284304513a  6631896 main/binary-armhf/Packages.xz
 d79ff6bfe5f300ec5c2675ad5c4eef202d7c041bc280400c63d5c0424b969762       94 main/binary-armhf/Release
 fcf304bea2200b49358db69cd1635c4f4b1e3b2a6443d812460c3550f39780f0 33870550 main/binary-i386/Packages
 70f9c6b31bee296157f5333933794c821598548af12abdbd7e380ac36f8e0ae3  9051240 main/binary-i386/Packages.gz
 71cacb934dc4ab2e67a5ed215ccbc9836cf8d95687edec7e7fe8d3916e3b3fe8  6779128 main/binary-i386/Packages.xz
 52da7ec1b0d0661b9318e6eae9a1cdd673679bd3f1ac89a491628b1eee56dc20       93 main/binary-i386/Release
 59418c0662dbabd0d884c8dd261a16ef5ae5c89c5a6901a82d0af54eca0d92f4 32797474 main/binary-mips/Packages
 2149614170b22981f3ecf6d8ed47c584f70662b507e19b411f5d1253942b4b6f  8792469 main/binary-mips/Packages.gz
 be89a8810eee0afe78c40a0a7673a270486c3b4048d88bce9d35cfeeb733575e  6585720 main/binary-mips/Packages.xz
 8ccadf4cada6ade6c6faeef612f589048b8686d4e0a8a13a7fadf175481623cc       93 main/binary-mips/Release
 7f4fa76aed63799437e8582d67d134985fd0fc014f2e85e9c85b82b327090c33 32955178 main/binary-mipsel/Packages
 39f43cd499c6b6946b7d6bf62511d7d84f24f437304e9031063917dcf25734ee  8817773 main/binary-mipsel/Packages.gz
 3c458d27e3df6e8562eb3552856af8e684147b66635e9e84496477910cf90d40  6603996 main/binary-mipsel/Packages.xz
 474170e84702cb3726c274b6c5e4319bb4e8245c3d118e43242ab75173db1ae2       95 main/binary-mipsel/Release
 d8dbd12e4fc39f9b0d1924368a602e6b0408c02a43354f840181bf7aaf78aad3 33428595 main/binary-powerpc/Packages
 7b27279164051b0d5afbaa6298634baf4f26fb5af8c9657730b1f3ed21301e96  8913905 main/binary-powerpc/Packages.gz
 903464d230ad563f839e6217aade430302e07f8448710c07f67cc2612270bacd  6674352 main/binary-powerpc/Packages.xz
 dfe11fbbc4fa1cb140645299e0c554c7220e690df4cc86daf9b12525e2cc9bb2       96 main/binary-powerpc/Release
 d0258b0778e69ac1ed09392b5b4dcfcec6fb16a7909e67f3f8c966272b5ae6b0 32449606 main/binary-ppc64el/Packages
 f3eea261a53a39f3491b255dd60653f797c3819ff7edcfdc6890e56730bd5976  8657717 main/binary-ppc64el/Packages.gz
 00637eba98e8f0296acad81044e976b7a20395087812b1adf6a5bd775d6ac856  6487204 main/binary-ppc64el/Packages.xz
 1ceb55027687449f8b1a2cf2f3cb68ae486ec19991fbfd60ce125f15ca1ed025       96 main/binary-ppc64el/Release
 9675973147daad7dbc831f66962cf9b18e227b39147c87318b3bf62cbbf868dc 32646123 main/binary-s390x/Packages
 e3c0010dcdc7dc89e4cb11144ed072110a4a05b0c25bead48642df4af2400eee  8747482 main/binary-s390x/Packages.gz
 97890a50449ea1f1dc91ec6e1bd5fc7e6d332b34cd21105903e47d8e8a7cd637  6550600 main/binary-s390x/Packages.xz
 2eb6b8366fa31faba6003a937d0538862ace418f51c2eaaf849b1cebe7e24020       94 main/binary-s390x/Release
 82d9193f3b3b2a1c552c827256e839a214e22534f4e343ff0ed06f2be8d1553b    67861 main/debian-installer/binary-all/Packages
 610793ba6a54526beaa1f892024d8ce692ffd65d217f2f6d13ea3aca168860e7    19852 main/debian-installer/binary-all/Packages.gz
 5b6191e0f9ead793f2066fd6371e533b7f94b4a84955d3412b1490d1f32e0725    17168 main/debian-installer/binary-all/Packages.xz
 14419ac338393d627e857115b7528c0665fe12eae1d8330f9ee809b0955ccfa3       92 main/debian-installer/binary-all/Release
 570c1ac4b1af59d21a67e1c63067c8ee85b82790c3e3403796348b20e5cc7a75   238176 main/debian-installer/binary-amd64/Packages
 f4acb31ccf10777b398c4bfd99949f46fb25ca7a12c837721a0560a382dc6871    68677 main/debian-installer/binary-amd64/Packages.gz
 519a3eeb7ba07415bcd094008114050f321f1a17b0f2cf51b881f2acacba4990    57148 main/debian-installer/binary-amd64/Packages.xz
 2c4f0f20a2503fdb19c68d2c37a6bde9cab84a595408d8d01cef80bb6927525c       94 main/debian-installer/binary-amd64/Release
 d5ea1986ccac0633e885c55d9f3652e8fcffb91757fdfca51f69ba1c24ee4692   221242 main/debian-installer/binary-arm64/Packages
 06c10266b8655f9fdf5ae94fd68dd5d9b6973cbcab3efc7f43ed91b7054b3134    63951 main/debian-installer/binary-arm64/Packages.gz
 48fc5c4fbc275d101f62e78a766b9f6127aa6f6c666c3e096bac30157f675515    53708 main/debian-installer/binary-arm64/Packages.xz
 4eba3bfaead4bec4aabb8b9fc964b843e69b8e2da567bbadc73e62f7b2f1f562       94 main/debian-installer/binary-arm64/Release
 d19b1fa3a94e1675ae9bb82ef0b97dc4eff83c00a0c1f282019c012654c13386   264220 main/debian-installer/binary-armel/Packages
 01ef8b980d0edc49418a23b8e43ece1d6c206d732c5c32cf4610785d9df886ab    72013 main/debian-installer/binary-armel/Packages.gz
 6a0d98585258e8ff3546e785f2f4cc0d9908ffe94c29a61bd6c58542c22f15b0    60384 main/debian-installer/binary-armel/Packages.xz
 e6afdec88f50b342ea5be05e15100c34cc9de4737633d02310b7a4fc05e858d3       94 main/debian-installer/binary-armel/Release
 7bfb4800002653d0cca41da94371ad1cd871a680a083aa24d5f35f6fead7b0de   223152 main/debian-installer/binary-armhf/Packages
 d8cbbfe5e3392f110196eb41598ab73bedff00b3a86b545bcfc36471e3c0d846    65038 main/debian-installer/binary-armhf/Packages.gz
 66a7ef1ed8097a7a85ff41e60e6175f521c5bfe9f198314aec1c442e4d1844c6    54336 main/debian-installer/binary-armhf/Packages.xz
 d79ff6bfe5f300ec5c2675ad5c4eef202d7c041bc280400c63d5c0424b969762       94 main/debian-installer/binary-armhf/Release
 467c1b6385c7b7bc7a2fc45e6418fd03d3351af6fd51127d73049b1232ffcfc4   276184 main/debian-installer/binary-i386/Packages
 461ffa9632e2fbc9bd9e4d9522f6be7281028dd2ddcba18a7ae192ca1deafac9    75214 main/debian-installer/binary-i386/Packages.gz
 83c7734db44b7390dc817503a3950af8e1006c2f7c844cc8b38061766e36e110    62864 main/debian-installer/binary-i386/Packages.xz
 52da7ec1b0d0661b9318e6eae9a1cdd673679bd3f1ac89a491628b1eee56dc20       93 main/debian-installer/binary-i386/Release
 d792a1c7bc42f2fcb15c2fc056cefe5adb4f923547cfb77686da4a63a25cced5   311838 main/debian-installer/binary-mips/Packages
 9b61f39f1ec7132872aef83e857de000bc43e15d866aa256c5b12cd08ac88946    80395 main/debian-installer/binary-mips/Packages.gz
 c4af527ba89b0a3b360103d862f276b0cb815178ca2d12c041bfe1d98ff89546    67344 main/debian-installer/binary-mips/Packages.xz
 8ccadf4cada6ade6c6faeef612f589048b8686d4e0a8a13a7fadf175481623cc       93 main/debian-installer/binary-mips/Release
 db2e5324f463e409d7f97337eacd412901e099236669993d19eaa72d5202b141   355242 main/debian-installer/binary-mipsel/Packages
 09cbe8f8080be7a94ed6702d4e82b42e2c71d011ba06eecc43238495369fb5a1    86928 main/debian-installer/binary-mipsel/Packages.gz
 967bcf6ae61d6cb6ed3975fa4558703076130226711ec6a161b62f2330e444db    72992 main/debian-installer/binary-mipsel/Packages.xz
 474170e84702cb3726c274b6c5e4319bb4e8245c3d118e43242ab75173db1ae2       95 main/debian-installer/binary-mipsel/Release
 a622f74afb2c29af7989ab14e1c8c12b0379fe86fd49884cf6157209594ead93   268225 main/debian-installer/binary-powerpc/Packages
 509839fa45b187c22f22c26ac055be39d4b1a7d570b882c68b6bab361cc07cd4    72930 main/debian-installer/binary-powerpc/Packages.gz
 f5040aacf6154bf4657586ab0f4f5ebeb40b8e296741e901ca8bd28a1e04c6a5    61236 main/debian-installer/binary-powerpc/Packages.xz
 dfe11fbbc4fa1cb140645299e0c554c7220e690df4cc86daf9b12525e2cc9bb2       96 main/debian-installer/binary-powerpc/Release
 bf69ba6f9971c3ca506a7878543e01f8865bcb539f235ff64542f20e304e45a9   225934 main/debian-installer/binary-ppc64el/Packages
 9dc87422bb13df57fe1f0252d8d5722b8bae18a80bd7ba6a835fffaaa2ecbeb0    64315 main/debian-installer/binary-ppc64el/Packages.gz
 0d4a76fcf3ce3e9e219f11b1cb9fca7bcab37e6daf928ca2af4b466482f90564    54260 main/debian-installer/binary-ppc64el/Packages.xz
 1ceb55027687449f8b1a2cf2f3cb68ae486ec19991fbfd60ce125f15ca1ed025       96 main/debian-installer/binary-ppc64el/Release
 5f7894cea3d9cebc56dfc67f354072484c891029eebbb01bd911dd3621ac518d   206467 main/debian-installer/binary-s390x/Packages
 5d6bf7132264d6b9aa171ec2d6b8819571f30eb444ba03a205a1c16c712fab74    60857 main/debian-installer/binary-s390x/Packages.gz
 905615d4323e176d6ad4ca9cf8644c62099ebf4dd508975d7b8958cd98b514ed    51020 main/debian-installer/binary-s390x/Packages.xz
 2eb6b8366fa31faba6003a937d0538862ace418f51c2eaaf849b1cebe7e24020       94 main/debian-installer/binary-s390x/Release
 ca13ee7f00c1f367d2f9abfe10432adc3b04248672b2e52dbdfb873bd6b62d46     9627 main/i18n/Translation-ca
 62a84909c0bac44a47e3eb5b96261c8da467ecd87693c84fce6e15dac761a6e0     3865 main/i18n/Translation-ca.bz2
 b0ce634e960a09c62522e7a9fef5ac34fe7f8a6a1bf74258ac33053c2c24f241  1893411 main/i18n/Translation-cs
 1f0bc9d9b42f9e0f7052581cf3c4d1a3b2647c2a9bab2ba3dcf44d98f2ee3b86   494559 main/i18n/Translation-cs.bz2
 dcc68a6a3d02e1f41b3d6533eb3c9e8f65af0ec09adccb1ca3a3cb570d5b2c38 13166981 main/i18n/Translation-da
 4beb5f9b7259cfd0482a6990798cfd92d98447a2ef03d916b171416e287caf6f  2901281 main/i18n/Translation-da.bz2
 4ab09f27d78d88db21e569229e538d367c17f51a6457aa1d211739f43b42be83  7664182 main/i18n/Translation-de
 eb2877dc80cd318b5b64cdbf219c9592e908efff167b76f5057fa376aadea2cf  1754517 main/i18n/Translation-de.bz2
 284169348b8bd4e0de4cc5641eeb05577e80d2bd736452e454976c052cf3cbe2     1347 main/i18n/Translation-de_DE
 481a435ad350105b74c4972859c44f447b7a8b5edea0d42f6dd635792e00a461      830 main/i18n/Translation-de_DE.bz2
 9f9abe444c71a7c8c9541f626211c000e4f311c8ea0a9061ff125c53ce18a210     8287 main/i18n/Translation-el
 8e4906ba999712818c7839947cfcd30d3575e7e6462d7c3c2a230f9ce84defc0     1896 main/i18n/Translation-el.bz2
 46201e3cab64b715856b6007a833d108446f0fbbf00fdf48559497f7f827dd8d 22326673 main/i18n/Translation-en
 fece8575e5715b5dd36e1abb6328ef658b78c65294448848ad728597031192f9  4582093 main/i18n/Translation-en.bz2
 57cbd7113d483fc4954c7f8f94d1fca2c6fe593d8edab0c880113637c4c2fb68     2739 main/i18n/Translation-eo
 3590dc840a339747c889397d5b37a9b7566d017ca1c64a441f2fe719c88ff6c3     1344 main/i18n/Translation-eo.bz2
 f1c592079a87ae0aa26db77961a0c8233d3cd0b4cefa36a1ff0694b956a145f0  1278186 main/i18n/Translation-es
 a06bf3a9408030cc44562e0dd25977b6b6986a29bff242cd5163dd3daf33a499   314368 main/i18n/Translation-es.bz2
 6774f0e7c9b52f064b6b029f159bcca7eb4ed1824c440d356a5d4f8347b769d9    17288 main/i18n/Translation-eu
 06501e6280985f7acdb4c6b901064246242904f2f2a73d46168f8203fc9d996e     6460 main/i18n/Translation-eu.bz2
 79d6ea8041b340a9ed7bfffe3b143e4a222e46e54fa623230b751cc5f250479b   390801 main/i18n/Translation-fi
 27466e4e7c05361e4f400bd1ca52211f36b14bec3fdf26e1bbfe2b673e96b176   108973 main/i18n/Translation-fi.bz2
 d8e9564b44bf1b3dedb9c5f50ab238b7005af5880f5be8c9f76251c0434b29c1  3701781 main/i18n/Translation-fr
 06433a6aa16bf695523d486818b381ff07f5568de239c7a6f72ab1639b807b9e   846329 main/i18n/Translation-fr.bz2
 c945037f623a4c46be03ca8bf22f29246c275c7f20dd552f6668a56270ad2055    14962 main/i18n/Translation-hr
 00ad39dc031364bb77848530e87c9ffee94aa43f3fd2dde54526bbcad62f839f     5841 main/i18n/Translation-hr.bz2
 c9214c0bbf86e57d0823ee5968658eca65d4cc3019e541595d0650aa888bfbbd   101201 main/i18n/Translation-hu
 cf71a62b4de459477e030cb601ea8a1517e0e67f4662c8fcd13075406896f4bc    33209 main/i18n/Translation-hu.bz2
 a57a7f9bcf3dc7707e92df28176161ae75b84a2ca903e7ee80bdfdd0ac89a0e6     7753 main/i18n/Translation-id
 a4e33fbc299198efcfc47c3df98c55c95197667099caf7dc2ee455309cbb3761     3093 main/i18n/Translation-id.bz2
 182e8212693cc8bccba98500b3f5e7c3a25a1a8715a78590eece48c0df6e93d8 17993956 main/i18n/Translation-it
 1ba10f2b7864ee34c9a716e4e8778c11530dce66b17d76b71fa31c0c9fd13722  3656485 main/i18n/Translation-it.bz2
 46142d62938cfd82a8e848ad652b7eb1ad5e598a4ef72e8174669ec970362cb2  4376703 main/i18n/Translation-ja
 56b68499d01ffc80d03f833aafdc0ea272ffb3876d90061ca9f39ded21fd8fd6   794698 main/i18n/Translation-ja.bz2
 9e870f2c80d2ae29ec5b8aaf979478b21dba6946781c842290c9d01909a37db5    21141 main/i18n/Translation-km
 633de64ac0e312e971274b0b12a210b3796755d3436aa797cd38dc25707c8eb5     3796 main/i18n/Translation-km.bz2
 dd6ea15ad5bad66b3cb2ecca051c197f00afaf4d0eee2f516cdf6edf7d39eb0d   924206 main/i18n/Translation-ko
 dd1c2e78c63af61cfba4407fd3843f5ca73593788d10689cb6b89d9f64908920   204449 main/i18n/Translation-ko.bz2
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 main/i18n/Translation-ml
 d3dda84eb03b9738d118eb2be78e246106900493c0ae07819ad60815134a8058       14 main/i18n/Translation-ml.bz2
 917a62d4d2bd8fb889d77caae2e4ff39abb23410919ecdc553e8aa4f1854da36     2325 main/i18n/Translation-nb
 487379711cdb467cfdc7ff412a0a844796bc8371252b051fc1b325f9ac90832d     1302 main/i18n/Translation-nb.bz2
 857b92b89f3dcc4c8e5dd1645a097349f12cb999c51b1f253874d3972993f604   267344 main/i18n/Translation-nl
 d9064f38c767571fd6367098e29eef4217945e85a79fea02bd7c16deb01f0650    74095 main/i18n/Translation-nl.bz2
 1332a460e0a348e929e9782c4137df27c12316d944ce5864ff5673cf8e934f2c  2490134 main/i18n/Translation-pl
 e8d23cec4050bb1b781b630cb1c92400c6ed7f981a252e28f2f8758b491b5ef5   574404 main/i18n/Translation-pl.bz2
 7e31f6cee54089dc34b43aa82253a920a135f091671822699628c16b35b2967d  1712133 main/i18n/Translation-pt
 08b50c66c04bb5fb1fcd352be8e3d24336dcc12248613fe9a2be5e46d25eb213   413142 main/i18n/Translation-pt.bz2
 05ee9a782e0367b5eca29c77960d09b2077bd1691c562b984af0fcd9d78b24bc  3363311 main/i18n/Translation-pt_BR
 42a33e4ad7503a6b320aa1f0928e69106b24a55a65bfdf6cb44adafe2363708d   802754 main/i18n/Translation-pt_BR.bz2
 00908d40a00314c1ceaf85a68550d22995a466bf22ebe9ee2d6ad5045e18734e     2949 main/i18n/Translation-ro
 93cee00ae5cf8a56439faf45ce383d73376e7628e4d6394c463363b52c24eb32     1463 main/i18n/Translation-ro.bz2
 e4be1494fb7a568295a28d24d14b377db7fd287c10a3fde4bf11b61bd5dd7c18  2684275 main/i18n/Translation-ru
 0d90506d320340ab59d92780ca4c34a7d37c91755c0619a2184381b837929c28   437612 main/i18n/Translation-ru.bz2
 b1b1632a02ad05e5dd5c6d287be67bbd046731835704c7c4c1b02423f2da6dea  2106842 main/i18n/Translation-sk
 ca0a6fca2846922673b3a8421915c495069ad1d5ed9bdd1cabcf473596e09af4   509104 main/i18n/Translation-sk.bz2
 c87a210cbc3b4710776da955b316cc83499d9fcba074d9a288c6ea49ff2099a5   455684 main/i18n/Translation-sr
 e93b90d3fcd156ac01b376fb3cf865adbe453f66db6e9a810202e46783525a86    81894 main/i18n/Translation-sr.bz2
 6fb6c24f5372962a8bf84b8ca9578e84b311e3a13a0db71ab6c38230d58de96d   132973 main/i18n/Translation-sv
 2699aef46d645591898b46c8a6f2aaf4f9eb0b00cd1fce843e662ebeeac38708    40142 main/i18n/Translation-sv.bz2
 926b4a85b107381c03f2ae77007d2a9e628b1da209a654ad190d720adfbdd546      902 main/i18n/Translation-tr
 b53b66bec94d548702370107afb1cec937abbd7b5b29ed47e5f64c1f7416414d      530 main/i18n/Translation-tr.bz2
 f4e700ad4c33191edefd7860b7633ffcc9823fba80c721fa37738a0b02b6a394  4630335 main/i18n/Translation-uk
 1ea3aa206d3e357270ad44b987f6a2f04b3ae301caa3c785c28b4bc99f6d1b77   734132 main/i18n/Translation-uk.bz2
 ef3f62a1e4f1021547f6dc6063265bab18a12d83a28a07a65e532791a947c741    36422 main/i18n/Translation-vi
 51f3f979f3229486bba6b577f0d8bc7b900cd0c727642a0553f924fb138df987    10243 main/i18n/Translation-vi.bz2
 ba5bf385ecf4799de85e8ab873c233fe0478bbdd30611baf528aea51ae2b97d2     2799 main/i18n/Translation-zh
 ebb2e1910c3096f393f46f4fb6b1316e571d805d654c2d97d0ea92ddb2229424     1526 main/i18n/Translation-zh.bz2
 1dc43dad693fb97fd97527e0dbaf27d60f48e7d2cc895c5f74d444427c6e9f50   367410 main/i18n/Translation-zh_CN
 736fd17216a423f88d3d1344ce11c9b6f3a02dadbd1cb959e9e6f5b10d51f695   100783 main/i18n/Translation-zh_CN.bz2
 75679567fe3b0718d5057b207946056e1eb4a468f7208b1b8f63f1b35bdc7427    64019 main/i18n/Translation-zh_TW
 4c89775758f1e6c2f333ccdf92f16292e0166815e4f27481e81ef7013e52da3b    22485 main/i18n/Translation-zh_TW.bz2
 afd35b783a6c4a5f6d2b2d868f24f832211a60205d906fcaf2e022306c650905    53815 main/installer-amd64/20150422+deb8u4+b2/images/MD5SUMS
 c1da32892cf0b2a881e415516b06bcafd9cf2995f2e40135a6ed2906ced50da7    72131 main/installer-amd64/20150422+deb8u4+b2/images/SHA256SUMS
 782fc37c049313ea581c76d8a06d10d894532edd954b2ca85bd0197b3c30dad1    53815 main/installer-amd64/20150422/images/MD5SUMS
 17abd745a3e8d79d4bc22dd9f5bded89b1841b7b991eaaf4a105bcfcc835294d    72131 main/installer-amd64/20150422/images/SHA256SUMS
 afd35b783a6c4a5f6d2b2d868f24f832211a60205d906fcaf2e022306c650905    53815 main/installer-amd64/current/images/MD5SUMS
 c1da32892cf0b2a881e415516b06bcafd9cf2995f2e40135a6ed2906ced50da7    72131 main/installer-amd64/current/images/SHA256SUMS
 c443ee2ee1b19bcd7ef463326cf14ab804fbba9806461d243bfb2bd79ef22061    19148 main/installer-arm64/20150422+deb8u4+b2/images/MD5SUMS
 cc2c4cd19ee6750d3f9d8d3dd9b811e7c8c2d2acea906b6fd719ab947a21bf92    25912 main/installer-arm64/20150422+deb8u4+b2/images/SHA256SUMS
 47a2f89ca82421b20984d38888d48cd5200dd676ca51a9342476bc62c812379c    19148 main/installer-arm64/20150422/images/MD5SUMS
 08af7315fd23120f3e8ad1af11ac2730a455d6d3b6ed673f427ee003068adf34    25912 main/installer-arm64/20150422/images/SHA256SUMS
 c443ee2ee1b19bcd7ef463326cf14ab804fbba9806461d243bfb2bd79ef22061    19148 main/installer-arm64/current/images/MD5SUMS
 cc2c4cd19ee6750d3f9d8d3dd9b811e7c8c2d2acea906b6fd719ab947a21bf92    25912 main/installer-arm64/current/images/SHA256SUMS
 8cdcceb6ba54650edc2f0d5d72ec059c6b729874695746d8553aaed4f940fbb0    11608 main/installer-armel/20150422+deb8u4+b2/images/MD5SUMS
 b926be766d034a56dd781dc122fd3eb6ae3b066dc55a74b244da5bd6df6e4ac8    16324 main/installer-armel/20150422+deb8u4+b2/images/SHA256SUMS
 fdbf3f1cd1be6e4192103e2af7e0d7df528af5c3786fd04f5ccf4ed9dc47529a     8985 main/installer-armel/20150422/images/MD5SUMS
 8a5e9f96a5574c8082ff1949b77a49b97593274fb4ee9d188609b2c849d534c1    12645 main/installer-armel/20150422/images/SHA256SUMS
 8cdcceb6ba54650edc2f0d5d72ec059c6b729874695746d8553aaed4f940fbb0    11608 main/installer-armel/current/images/MD5SUMS
 b926be766d034a56dd781dc122fd3eb6ae3b066dc55a74b244da5bd6df6e4ac8    16324 main/installer-armel/current/images/SHA256SUMS
 ef338b99324e8af79b85daf1cfe2e144ae4cff7e95d1b5b50236fe16dadafe03    19599 main/installer-armhf/20150422+deb8u4+b2/images/MD5SUMS
 dd59c9ebc39c0fa901265fdd835dfd16339c2ade5b10e7fed2447e132dccc4c0    28379 main/installer-armhf/20150422+deb8u4+b2/images/SHA256SUMS
 8ed796ad892478a3d68d7d6e5ab0d4f0106a45a48a1bbd0c812474f029fde7b7    19599 main/installer-armhf/20150422/images/MD5SUMS
 983d76be48052b2f1bf31cb984ec93f47468da67cbc4284288a0ed2a9a6c93ac    28379 main/installer-armhf/20150422/images/SHA256SUMS
 ef338b99324e8af79b85daf1cfe2e144ae4cff7e95d1b5b50236fe16dadafe03    19599 main/installer-armhf/current/images/MD5SUMS
 dd59c9ebc39c0fa901265fdd835dfd16339c2ade5b10e7fed2447e132dccc4c0    28379 main/installer-armhf/current/images/SHA256SUMS
 398d264b1733003e3d7e25a7e1a9b667ec828bb0c5faaba9a4f330d5d96cda1f    52495 main/installer-i386/20150422+deb8u4+b2/images/MD5SUMS
 9dad71364ed9d1c1362ee3c9e0997bb3280882e3fdfb24d61879c788691b5edf    70875 main/installer-i386/20150422+deb8u4+b2/images/SHA256SUMS
 0c1498d49c3cbba310a31b29563273272237c49effb30bbb57a3e618e03bd212    52495 main/installer-i386/20150422/images/MD5SUMS
 8ab3f5b9dbf85cd95ca3fdd96e47f5145f9119b2501bdb4774e9da644e74c8e6    70875 main/installer-i386/20150422/images/SHA256SUMS
 398d264b1733003e3d7e25a7e1a9b667ec828bb0c5faaba9a4f330d5d96cda1f    52495 main/installer-i386/current/images/MD5SUMS
 9dad71364ed9d1c1362ee3c9e0997bb3280882e3fdfb24d61879c788691b5edf    70875 main/installer-i386/current/images/SHA256SUMS
 b66aefae16097b075b1bad1936761445ab7ddfb7f3134dfee07b243a3df35940      940 main/installer-mips/20150422+deb8u4+b2/images/MD5SUMS
 cc5f5b504e319698215876ca64b9d6e86907c22779909b92cd5cbf0c5455617c     1496 main/installer-mips/20150422+deb8u4+b2/images/SHA256SUMS
 0ba9585385f7e4d35f74db8d234e14e0ad97de6c2b1e58f4335b9cb61cbf4d24      940 main/installer-mips/20150422/images/MD5SUMS
 637d65df2638cb80ca893ba306af0478db47bd105b8a56a0f2fbf49c8e8b1e1d     1496 main/installer-mips/20150422/images/SHA256SUMS
 b66aefae16097b075b1bad1936761445ab7ddfb7f3134dfee07b243a3df35940      940 main/installer-mips/current/images/MD5SUMS
 cc5f5b504e319698215876ca64b9d6e86907c22779909b92cd5cbf0c5455617c     1496 main/installer-mips/current/images/SHA256SUMS
 5c397f7d794c03267a313f8beeedbecf6b89cee655564445f414078dcde82eff     1213 main/installer-mipsel/20150422+deb8u4+b2/images/MD5SUMS
 b97e3bd2bccc206b4f857ee8c6d91ca4827399aa07a5711683b849a83a4577ca     1865 main/installer-mipsel/20150422+deb8u4+b2/images/SHA256SUMS
 d6c364dc4ef14cc10ff0db0f30665f597417574549444c2bda4ddde8e6ba49ca     1213 main/installer-mipsel/20150422/images/MD5SUMS
 07340180253b71ca03bd55ae026fdc5959741054f030a11f5646d06429211bf1     1865 main/installer-mipsel/20150422/images/SHA256SUMS
 5c397f7d794c03267a313f8beeedbecf6b89cee655564445f414078dcde82eff     1213 main/installer-mipsel/current/images/MD5SUMS
 b97e3bd2bccc206b4f857ee8c6d91ca4827399aa07a5711683b849a83a4577ca     1865 main/installer-mipsel/current/images/SHA256SUMS
 ae0b4594d8016f103c7ddd96e86378b3ed265604b0ab3c3c1a4b1d4c8b2b46b2     2128 main/installer-powerpc/20150422+deb8u4+b2/images/MD5SUMS
 df2ae009c69b3808bc650881151224ff6b44eeef71bff7918f4d98ae508137b6     3292 main/installer-powerpc/20150422+deb8u4+b2/images/SHA256SUMS
 db19e3daaca03a155831e9b588810291489f0e82be200fab0c4cbd36d3294efa     2128 main/installer-powerpc/20150422/images/MD5SUMS
 581181933b42af5df34ae56e335184a8677aa4e4bd6d2f5a6f2566b1e84fdc4a     3292 main/installer-powerpc/20150422/images/SHA256SUMS
 ae0b4594d8016f103c7ddd96e86378b3ed265604b0ab3c3c1a4b1d4c8b2b46b2     2128 main/installer-powerpc/current/images/MD5SUMS
 df2ae009c69b3808bc650881151224ff6b44eeef71bff7918f4d98ae508137b6     3292 main/installer-powerpc/current/images/SHA256SUMS
 623d28d22b854a7d39636ce39276c349863271b3c30dd3b05d423c59b016e0cd      576 main/installer-ppc64el/20150422+deb8u4+b2/images/MD5SUMS
 c95c402280c99e118d5b1edf060a2554946a5d98ef750440172fa521a82d9fe8      972 main/installer-ppc64el/20150422+deb8u4+b2/images/SHA256SUMS
 1ddb9a9a243149f93822c8b14e1f1ed2ef69c46c79c2cc2a01c20647346f86c6      576 main/installer-ppc64el/20150422/images/MD5SUMS
 914330f644847bb5401fa3160cfe2d79e50c02e3df91bffb1085081a51db23ae      972 main/installer-ppc64el/20150422/images/SHA256SUMS
 623d28d22b854a7d39636ce39276c349863271b3c30dd3b05d423c59b016e0cd      576 main/installer-ppc64el/current/images/MD5SUMS
 c95c402280c99e118d5b1edf060a2554946a5d98ef750440172fa521a82d9fe8      972 main/installer-ppc64el/current/images/SHA256SUMS
 c946108f98aede6de9f98e498859ca5f20473ff12ee36ed97ccb96e61663bf6c      374 main/installer-s390x/20150422+deb8u4+b2/images/MD5SUMS
 c83410f52348e999615d7b421b5726cad79e39144ed07c187fe9783fa1e65b14      674 main/installer-s390x/20150422+deb8u4+b2/images/SHA256SUMS
 376bfb191054107631fe8de2dcedcefa14b35a6e957a4210fe8b76a79a16471e      374 main/installer-s390x/20150422/images/MD5SUMS
 f8a4b1aaab7b502945db265c1560a47a641609753b77ed61587d68b3e0fd7218      674 main/installer-s390x/20150422/images/SHA256SUMS
 c946108f98aede6de9f98e498859ca5f20473ff12ee36ed97ccb96e61663bf6c      374 main/installer-s390x/current/images/MD5SUMS
 c83410f52348e999615d7b421b5726cad79e39144ed07c187fe9783fa1e65b14      674 main/installer-s390x/current/images/SHA256SUMS
 50c05430b51f7edc853700bdece2ed9ce42e8f898c9f64cf6c0c5bb0d7554cc6       95 main/source/Release
 ba4cfd2c072bb3e10a6991f3340aca2238c579a0eabaa807981faef71e538af9 32518618 main/source/Sources
 8ac7641ad932de33558e2305ce13197b8c3332f8f04e7621507e52fd0eaf36c8  9156980 main/source/Sources.gz
 d4515f88b4a63cada8c63685c8d7fb120c9581124b6d9738e72d9ea0d741953d  7055888 main/source/Sources.xz
 5b0e04009685ea5979409c7ca705702d0d3d2bdbd102919b6bb298869368d33d 13875760 non-free/Contents-amd64
 46a5054bc39eb811fdcf683ffc32017d266d072e34f40d3c8812ca15f9589385   780181 non-free/Contents-amd64.gz
 5c082d207371582bcdd61cbdc37792b6b43f2824471ddd6a2e8bfd9921c8c1d2 12901760 non-free/Contents-arm64
 e551fc27b2b983fc208cfa21998a2f99709a78bfcf40afa28e3134773f85d405   702742 non-free/Contents-arm64.gz
 161f8b4d625870ce8a3be764ad8753d908d4b7df9d9db274e0ca1393df86c3f2 12974290 non-free/Contents-armel
 73b954a0274abc27f468abc6282cfd45c60eb5bd7a0c1d850e5acb92c22fe630   709306 non-free/Contents-armel.gz
 e3d9b6f100423193060e1c5891559aba6e581a579a31a0fa8870e87b1516a921 12999757 non-free/Contents-armhf
 a73cc9dd769c28a7f29ed93e1189f9c46a0cc12359749b5740c0eba7e82b5922   711618 non-free/Contents-armhf.gz
 9778a742ec7a7ea0e4ef68625dcadd5defe01d9b6aaf287b13ec09ad3c92ac74 13788194 non-free/Contents-i386
 af70cb04472aa66e8519f42dae492ad9ddc2c0954fdbd8955c22f1d4f3f37de4   773196 non-free/Contents-i386.gz
 bfca354e545c817f06014572476f488fb9fa3f81f123e890035bea58e7bb881b 12988374 non-free/Contents-mips
 8278bbf60f6fd8f582f7e9d6de16591488816338529bc7107cabcb8bb7baaa51   711304 non-free/Contents-mips.gz
 a4b248d0ddf16ead980b84e64ac9ce67bf6577dd46841759178dcce92f469efc 12993364 non-free/Contents-mipsel
 d2dcd33c296842466a1bccba399cba5125e00f5c41a5d07b26eebbe87768d1d2   711343 non-free/Contents-mipsel.gz
 029f805819fa854ba1ee67fe70c3e6d774852d280d49b228c584ef9321c030c6 12984126 non-free/Contents-powerpc
 48ed1703f82273932a27ddf22cf51dc6fa1c89c0bc44ec3e741231abacd19f06   710607 non-free/Contents-powerpc.gz
 c4289b2ba79ddaa33f23979116a4e14f266b10193e31604e5b091e27913b4312 12948073 non-free/Contents-ppc64el
 f2a26bd4fa6402ac22b543a4d6e0b4f454a045a7c51efb5920abb3a09d82de7d   707183 non-free/Contents-ppc64el.gz
 8a5289762de75a97ec0a37d6c14c26b269a47734b245de229273cbe6301d85f3 12985547 non-free/Contents-s390x
 dc9464233f531832d79d270b716594fe788dc08e4eb9a6c1ba22059f4a213229   710715 non-free/Contents-s390x.gz
 ee9350e8e77f054953681eacf6c2758b4ed6f3e13225c93ad7c8f51e7fbd62b8  8565950 non-free/Contents-source
 0ea6be298c9b7a60df61c8b41ae6b725ce1477b7a6a15711ac3755f469fc45ab   917043 non-free/Contents-source.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-amd64
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-amd64.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-arm64
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-arm64.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-armel
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-armel.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-armhf
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-armhf.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-i386
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-i386.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-mips
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-mips.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-mipsel
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-mipsel.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-powerpc
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-powerpc.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-ppc64el
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-ppc64el.gz
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/Contents-udeb-s390x
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/Contents-udeb-s390x.gz
 a2d28f70e47c0dc2583191a4a521158365336c15fd1bbf114191137ad7a70c83   190308 non-free/binary-all/Packages
 b6c61781c56b2e143d5af30a6ded280dc1805e93316784a2dab369ab5aa6af32    56603 non-free/binary-all/Packages.gz
 aacafc48451909a2bfae2972557ef47b6c99cdce9df305b87ed978a96bf9c3e2    47968 non-free/binary-all/Packages.xz
 651a82c23588bc0f9462b2bf7eb97364fdbade1607bd2ec95bb785b7d99615ff       96 non-free/binary-all/Release
 b750e1c20c1e45d74bff395c0a5561797f491e8f755526eb18524feb09ee2367   355467 non-free/binary-amd64/Packages
 347338b3877a6315192ff31b2f97dbb1ccf90d91390c023a3965790fa3d645be   101206 non-free/binary-amd64/Packages.gz
 d3ffd30e7e344aa9dfa629b883f186a8848076f60f07719ee69a8e0ac25bf467    83648 non-free/binary-amd64/Packages.xz
 d61a8c40bd8690c639c7e14fbaa681ab3a99059707c81b5fa851c60e393aa23e       98 non-free/binary-amd64/Release
 9e7cd78afd5d8c3c15aaf0cb1684d47fed383f732797bf054de790b400b6ce7a   230493 non-free/binary-arm64/Packages
 004c67a442a1c94f95125dbad68251e5ff295e6f2c8e08f5b3367351f2ff6d73    68723 non-free/binary-arm64/Packages.gz
 b201e227c1e1a13830623908f1259e4de392db6d27bc9b7662aec6b4dc3e3478    57432 non-free/binary-arm64/Packages.xz
 d04389382de9639bd47464c8d077cc7d4406b8fabf1be08f7c6a591880e40224       98 non-free/binary-arm64/Release
 c1d4eef3e0b8e8a3331ce043fa00d32b18b44c4822ade5f41d3d1b425b6fa450   239168 non-free/binary-armel/Packages
 4da571973fb6a9321fa50c5da4a49fcd1a32c43712b8927ac9a1038a8e71543d    71096 non-free/binary-armel/Packages.gz
 57b293a61eb6854c8066b0da0be5b31a925da836d5f5c8aced18cd0c35457521    59656 non-free/binary-armel/Packages.xz
 af2a9f7ee556386a9a030687e44651af27a450b41abc9081c1fbf087ca446edd       98 non-free/binary-armel/Release
 6ce4f3cbaa6fae95a602bbb4ff5483c736f945f60d7764f5772276fc82029d63   255719 non-free/binary-armhf/Packages
 b5e34aaa687fb7c4d6a0dd9f3b9d21be25985d223511c4414897626b27c5a6fe    74897 non-free/binary-armhf/Packages.gz
 6f5982a0872d88bc9c015c7601cc23ece4341d001bdd9f3bb047fa5109352887    62484 non-free/binary-armhf/Packages.xz
 c5d641cd76ca3c61ee9482c9417a56a03b95234bad65b4e4736a4d44ae42fc0b       98 non-free/binary-armhf/Release
 59fb7d8f40382acb7085b67be59a2fbbd971cc90577ece402996a4e767ac3f7e   344437 non-free/binary-i386/Packages
 c825ec617fa95cb5a102eba2410a851c2c9ff188e44e8488f68b60d8dfc539aa    96963 non-free/binary-i386/Packages.gz
 3769dbfb248542ac7c80ccbf817bb1511f30e020d24330c885422207ef9c6da1    80432 non-free/binary-i386/Packages.xz
 11e62e553d97aec871072426b399a0c93b71c669c95abc41bd1e35fcc8e762ea       97 non-free/binary-i386/Release
 990095042b42d1ac31448739d7ce764b2dab5ad581e05976ffcef1c75a584b1c   240596 non-free/binary-mips/Packages
 1f533bf16999f58b1db8c39da5524112a718cc9a452a139be4f88ac7b748885e    71671 non-free/binary-mips/Packages.gz
 5b9542642aadba7d022608a15d9f973a6f22a2955b1ac5f4e794b21b28be10c3    59864 non-free/binary-mips/Packages.xz
 3927a691f34c31af4cc17aeb482418bed3789a289d93bebbacfd8c1c6933419a       97 non-free/binary-mips/Release
 9b053d024e2317e2d93d05ddae236a28b0bbe321d0c49b09c805b9088709fe28   243099 non-free/binary-mipsel/Packages
 1262fadd494657515b6d2e8870266193149be48f513e666901c0cad0492ce5f9    72082 non-free/binary-mipsel/Packages.gz
 3885b7ae894600ed12355248963c01d5d0b1ed1b98f873137c6698b381685d0b    60296 non-free/binary-mipsel/Packages.xz
 d686d14f8dc2c58e9e3849534b9d53efea9bb5db505401807bd72b6be9d7b436       99 non-free/binary-mipsel/Release
 867c44ab83224be5f329d89006714da2df5653e74cf763576c9f1b83ec5c89b4   239066 non-free/binary-powerpc/Packages
 d50c0fc43bd4e884b8ac310b2a1b5f63859dd658fd05aa1a30217006a950f2f6    71145 non-free/binary-powerpc/Packages.gz
 82468e69639df27c4e84e7d4bffca3e0fb627bf760fba6b6dd90c2bf0544e560    59496 non-free/binary-powerpc/Packages.xz
 db3b73c682de8e2e368e16352c7260dc3643f1567ce0075520a8dd932e67dc81      100 non-free/binary-powerpc/Release
 864370cdc4fc7fa1f458aaa7762f35917c31710a23ee6a2563996e40d58fb9c2   233710 non-free/binary-ppc64el/Packages
 a6e74cc0c0d9a01e6504be623a61702cd9bbe8198aee4f2ff8034bae43d75946    69140 non-free/binary-ppc64el/Packages.gz
 21b598419b07fb53042fdfaa0a8d9e86b75bf47f3407cf5ba8a55f098753e1d0    58056 non-free/binary-ppc64el/Packages.xz
 813c5be206cb6b81f7e149bd55943466cc984fdb88a6ec859de8ce2c962f64ae      100 non-free/binary-ppc64el/Release
 a3ea1d99b7de8dfe571b0287c545bd491d3f910fc56f0a217e7cdce9846a4e1c   239446 non-free/binary-s390x/Packages
 9536eab9816d351b3be83c0b64d18b89435a3674ccad4bd087b98a0c995825c3    71176 non-free/binary-s390x/Packages.gz
 edec45392a357eadce19ce50831046de85ff88b7598ad6b77a7558f4286dfa71    59472 non-free/binary-s390x/Packages.xz
 54e877b56c0118c277aea7009421d7f18f75d53207a6c8ea79b72720005cf417       98 non-free/binary-s390x/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-all/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-all/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-all/Packages.xz
 651a82c23588bc0f9462b2bf7eb97364fdbade1607bd2ec95bb785b7d99615ff       96 non-free/debian-installer/binary-all/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-amd64/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-amd64/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-amd64/Packages.xz
 d61a8c40bd8690c639c7e14fbaa681ab3a99059707c81b5fa851c60e393aa23e       98 non-free/debian-installer/binary-amd64/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-arm64/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-arm64/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-arm64/Packages.xz
 d04389382de9639bd47464c8d077cc7d4406b8fabf1be08f7c6a591880e40224       98 non-free/debian-installer/binary-arm64/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-armel/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-armel/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-armel/Packages.xz
 af2a9f7ee556386a9a030687e44651af27a450b41abc9081c1fbf087ca446edd       98 non-free/debian-installer/binary-armel/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-armhf/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-armhf/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-armhf/Packages.xz
 c5d641cd76ca3c61ee9482c9417a56a03b95234bad65b4e4736a4d44ae42fc0b       98 non-free/debian-installer/binary-armhf/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-i386/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-i386/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-i386/Packages.xz
 11e62e553d97aec871072426b399a0c93b71c669c95abc41bd1e35fcc8e762ea       97 non-free/debian-installer/binary-i386/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-mips/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-mips/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-mips/Packages.xz
 3927a691f34c31af4cc17aeb482418bed3789a289d93bebbacfd8c1c6933419a       97 non-free/debian-installer/binary-mips/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-mipsel/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-mipsel/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-mipsel/Packages.xz
 d686d14f8dc2c58e9e3849534b9d53efea9bb5db505401807bd72b6be9d7b436       99 non-free/debian-installer/binary-mipsel/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-powerpc/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-powerpc/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-powerpc/Packages.xz
 db3b73c682de8e2e368e16352c7260dc3643f1567ce0075520a8dd932e67dc81      100 non-free/debian-installer/binary-powerpc/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-ppc64el/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-ppc64el/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-ppc64el/Packages.xz
 813c5be206cb6b81f7e149bd55943466cc984fdb88a6ec859de8ce2c962f64ae      100 non-free/debian-installer/binary-ppc64el/Release
 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855        0 non-free/debian-installer/binary-s390x/Packages
 f61f27bd17de546264aa58f40f3aafaac7021e0ef69c17f6b1b4cd7664a037ec       20 non-free/debian-installer/binary-s390x/Packages.gz
 0040f94d11d0039505328a90b2ff48968db873e9e7967307631bf40ef5679275       32 non-free/debian-installer/binary-s390x/Packages.xz
 54e877b56c0118c277aea7009421d7f18f75d53207a6c8ea79b72720005cf417       98 non-free/debian-installer/binary-s390x/Release
 8ad17598683670c1e7347c5f946e8e3d108213451bdfe60f77fcc05c497cbcb0   309527 non-free/i18n/Translation-en
 56d042958a7a70e70917cdead2e264240555282c1500beb4bfd747104e3ad04c    72067 non-free/i18n/Translation-en.bz2
 a356804ee31ddf00864ea78f9f7e247e4d08a0f79cad122cc7518b347c256aaa       99 non-free/source/Release
 d725d2d08d6e851b2693a4584a8e0e6f09a5b3c1947a77be1f6e197044daa07c   397177 non-free/source/Sources
 f9883dbf3acd65ae652fb5b4a7d3cf31de78695450e6cdac887041f51345aa4a   119076 non-free/source/Sources.gz
 c8661d4926323ee7eba2db21f5d0c7978886137a0b8b122ca0231a408dc9ce08    99496 non-free/source/Sources.xz
`
	signature, err := key.Sign([]byte(signedFile))
	hashSample := "c8661d4926323ee7eba2db21f5d0c7978886137a0b8b122ca0231a408dc9ce08"
	round = monitor.NewTimeMeasure("apt_gpg")
	key.Verify([]byte(signedFile), signature)
	for i := 0; i < e.NumberOfInstalledPackages; i += 1 {
		if hashSample != "c8661d4926323ee7eba2db21f5d0c7978886137a0b8b122ca0231a408dc9ce08" {
			log.ErrFatal(errors.New("should never happen"))
		}
	}
	round.Record()

	return nil
}
